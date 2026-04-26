package test

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	goversion "github.com/hashicorp/go-version"
	"github.com/spf13/viper"
)

const (
	defaultLinodeRegion       = "us-ord"
	defaultLinodeInstanceType = "g6-standard-2"
	defaultLinodeImage        = "linode/ubuntu22.04"
	defaultLinodeNamespace    = "fleet-default"
)

type downstreamProvisioningConfig struct {
	ClusterName  string
	MachineName  string
	SecretName   string
	Namespace    string
	Region       string
	InstanceType string
	Image        string
	K3SVersion   string
	LinodeToken  string
}

type provisioningClusterStatus struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Status struct {
		Phase       string `json:"phase"`
		Ready       bool   `json:"ready"`
		ClusterName string `json:"clusterName"`
		Conditions  []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"conditions"`
	} `json:"status"`
}

type podList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	} `json:"items"`
}

func TestHAProvisionLinodeDownstream(t *testing.T) {
	requireExplicitLifecycleTest(t, "TestHAProvisionLinodeDownstream")
	setupConfig(t)

	linodeToken := strings.TrimSpace(os.Getenv("LINODE_TOKEN"))
	if linodeToken == "" {
		t.Skip("LINODE_TOKEN is not set; skipping Linode downstream provisioning")
	}

	totalHAs := viper.GetInt("total_has")
	if totalHAs < 1 {
		t.Fatal("total_has must be at least 1")
	}

	terraformOptions := getTerraformOptions(t, totalHAs)
	outputs := getTerraformOutputs(t, terraformOptions)
	if len(outputs) == 0 {
		t.Fatal("No outputs received from terraform")
	}

	runID := strings.TrimSpace(os.Getenv("GITHUB_RUN_ID"))
	if runID == "" {
		runID = strings.TrimSpace(os.Getenv("SIGNOFF_RUN_ID"))
	}
	namePrefix := strings.TrimSpace(os.Getenv("LINODE_CLUSTER_PREFIX"))
	namePrefix = downstreamClusterNamePrefix(namePrefix, runID)

	timeout := durationFromEnv("LINODE_DOWNSTREAM_TIMEOUT", 15*time.Minute)
	var wg sync.WaitGroup
	errCh := make(chan error, totalHAs)
	for i := 1; i <= totalHAs; i++ {
		instanceNum := i
		haOutputs := getHAOutputs(instanceNum, outputs)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := provisionLinodeDownstreamForHA(instanceNum, haOutputs, linodeToken, namePrefix, runID, timeout); err != nil {
				errCh <- err
			}
		}()
	}

	wg.Wait()
	close(errCh)

	var failures []string
	for err := range errCh {
		failures = append(failures, err.Error())
	}
	if len(failures) > 0 {
		t.Fatalf("Linode downstream provisioning failed:\n%s", strings.Join(failures, "\n"))
	}
}

func provisionLinodeDownstreamForHA(instanceNum int, haOutputs TerraformOutputs, linodeToken, namePrefix, runID string, timeout time.Duration) error {
	kubeconfigPath := filepath.Join(fmt.Sprintf("high-availability-%d", instanceNum), "kube_config.yaml")
	if _, err := os.Stat(kubeconfigPath); err != nil {
		return fmt.Errorf("kubeconfig not available for HA %d at %s: %w", instanceNum, kubeconfigPath, err)
	}

	if err := ensureLinodeNodeDriverActive(kubeconfigPath); err != nil {
		return err
	}

	suffix := randomHex(4)
	clusterName := dnsLabel(fmt.Sprintf("%s-ha%d-%s", namePrefix, instanceNum, suffix))
	if runID != "" {
		clusterName = dnsLabel(fmt.Sprintf("%s-%s-ha%d-%s", namePrefix, shortRunID(runID), instanceNum, suffix))
	}

	adminToken, err := createRancherAdminToken(haOutputs.RancherURL, viper.GetString("rancher.bootstrap_password"))
	if err != nil {
		return err
	}
	if err := configureRancherServerURL(haOutputs.RancherURL, adminToken); err != nil {
		return err
	}
	k3sVersion, err := resolveK3SDefaultVersion(haOutputs.RancherURL, adminToken)
	if err != nil {
		return err
	}

	cfg := downstreamProvisioningConfig{
		ClusterName:  clusterName,
		SecretName:   dnsLabel("cc-" + clusterName),
		Namespace:    defaultLinodeNamespace,
		Region:       envOrDefaultTrimmed("LINODE_REGION", defaultLinodeRegion),
		InstanceType: envOrDefaultTrimmed("LINODE_INSTANCE_TYPE", defaultLinodeInstanceType),
		Image:        envOrDefaultTrimmed("LINODE_IMAGE", defaultLinodeImage),
		K3SVersion:   k3sVersion,
		LinodeToken:  linodeToken,
	}

	log.Printf("[downstream][ha-%d] Creating one-node Linode K3s cluster %s on %s (%s, %s, %s)",
		instanceNum, cfg.ClusterName, clickableURL(haOutputs.RancherURL), cfg.K3SVersion, cfg.Region, cfg.InstanceType)

	if err := kubectlApply(kubeconfigPath, renderLinodeCredentialSecretManifest(cfg)); err != nil {
		return err
	}

	machineName, err := createLinodeMachineConfig(haOutputs.RancherURL, adminToken, cfg)
	if err != nil {
		_ = runKubectlDirect(kubeconfigPath, "delete", "secret", cfg.SecretName, "-n", "cattle-global-data", "--ignore-not-found=true")
		return err
	}
	cfg.MachineName = machineName
	log.Printf("[downstream][ha-%d] Created Linode machine config %s", instanceNum, cfg.MachineName)

	if err := kubectlApply(kubeconfigPath, renderLinodeDownstreamClusterManifest(cfg)); err != nil {
		_ = runKubectlDirect(kubeconfigPath, "delete", "linodeconfig.rke-machine-config.cattle.io", cfg.MachineName, "-n", cfg.Namespace, "--ignore-not-found=true")
		_ = runKubectlDirect(kubeconfigPath, "delete", "secret", cfg.SecretName, "-n", "cattle-global-data", "--ignore-not-found=true")
		return err
	}

	if err := writeDownstreamOutputs(instanceNum, cfg, haOutputs, ""); err != nil {
		return err
	}

	if err := waitForProvisioningClusterActive(kubeconfigPath, cfg.Namespace, cfg.ClusterName, timeout); err != nil {
		return err
	}
	status, err := getProvisioningClusterStatus(kubeconfigPath, cfg.Namespace, cfg.ClusterName)
	if err != nil {
		return err
	}
	managementClusterID := strings.TrimSpace(status.Status.ClusterName)
	if managementClusterID == "" {
		return fmt.Errorf("downstream cluster %s is active but status.clusterName is empty", cfg.ClusterName)
	}
	if _, err := writeDownstreamKubeconfig(instanceNum, cfg, haOutputs, managementClusterID); err != nil {
		return err
	}
	if err := writeDownstreamOutputs(instanceNum, cfg, haOutputs, managementClusterID); err != nil {
		return err
	}

	log.Printf("[downstream][ha-%d] Linode downstream cluster %s is active", instanceNum, cfg.ClusterName)
	return nil
}

func ensureLinodeNodeDriverActive(kubeconfigPath string) error {
	output, err := runKubectlOutput(kubeconfigPath, "get", "nodedriver.management.cattle.io", "linode", "-o", "json")
	if err != nil {
		return fmt.Errorf("linode node driver is not available: %w", err)
	}

	var driver struct {
		Spec struct {
			Active bool `json:"active"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte(output), &driver); err != nil {
		return fmt.Errorf("failed to parse Linode node driver: %w", err)
	}
	if driver.Spec.Active {
		return waitForLinodeMachineConfigAPI(kubeconfigPath, durationFromEnv("LINODE_DRIVER_TIMEOUT", 5*time.Minute))
	}

	log.Printf("[downstream] Activating Linode node driver")
	if err := runKubectlDirect(kubeconfigPath, "patch", "nodedriver.management.cattle.io", "linode", "--type=merge", "-p", `{"spec":{"active":true}}`); err != nil {
		return err
	}
	return waitForLinodeMachineConfigAPI(kubeconfigPath, durationFromEnv("LINODE_DRIVER_TIMEOUT", 5*time.Minute))
}

func waitForLinodeMachineConfigAPI(kubeconfigPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		output, err := runKubectlOutput(kubeconfigPath, "api-resources", "--api-group", "rke-machine-config.cattle.io", "-o", "name")
		if err == nil {
			for _, resource := range strings.Fields(output) {
				if resource == "linodeconfigs" || resource == "linodeconfigs.rke-machine-config.cattle.io" {
					return nil
				}
			}
			log.Printf("[downstream] Waiting for Linode machine config API; current resources: %s", strings.Join(strings.Fields(output), ", "))
		} else {
			log.Printf("[downstream] Waiting for Linode machine config API: %v", err)
		}
		time.Sleep(10 * time.Second)
	}
	return fmt.Errorf("timed out after %s waiting for Linode machine config API", timeout)
}

func TestHADeleteLinodeDownstream(t *testing.T) {
	requireExplicitLifecycleTest(t, "TestHADeleteLinodeDownstream")
	setupConfig(t)

	records, err := readDownstreamOutputRecords()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) == 0 {
		t.Skip("no downstream-ha-*.json files found; skipping Linode downstream cleanup")
	}

	timeout := durationFromEnv("LINODE_DOWNSTREAM_DELETE_TIMEOUT", 20*time.Minute)
	var wg sync.WaitGroup
	errCh := make(chan error, len(records))
	for _, record := range records {
		record := record
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := deleteLinodeDownstream(record, timeout); err != nil {
				errCh <- err
			}
		}()
	}

	wg.Wait()
	close(errCh)

	var failures []string
	for err := range errCh {
		failures = append(failures, err.Error())
	}
	if len(failures) > 0 {
		t.Fatalf("Linode downstream cleanup failed:\n%s", strings.Join(failures, "\n"))
	}
}

type downstreamOutputRecord struct {
	HAIndex             int    `json:"ha_index"`
	RancherHost         string `json:"rancher_host"`
	ClusterName         string `json:"cluster_name"`
	ManagementClusterID string `json:"management_cluster_id"`
	KubeconfigPath      string `json:"kubeconfig_path"`
	K3SVersion          string `json:"k3s_version"`
	LinodeRegion        string `json:"linode_region"`
	LinodeType          string `json:"linode_type"`
	LinodeImage         string `json:"linode_image"`
	MachineConfig       string `json:"machine_config"`
	SecretName          string `json:"secret_name"`
	Namespace           string `json:"namespace"`
}

func readDownstreamOutputRecords() ([]downstreamOutputRecord, error) {
	paths, err := filepath.Glob(automationOutputPath("downstream-ha-*.json"))
	if err != nil {
		return nil, err
	}

	records := make([]downstreamOutputRecord, 0, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var record downstreamOutputRecord
		if err := json.Unmarshal(data, &record); err != nil {
			return nil, fmt.Errorf("failed to parse %s: %w", path, err)
		}
		if record.ClusterName == "" || record.HAIndex < 1 {
			return nil, fmt.Errorf("invalid downstream output record %s", path)
		}
		if record.Namespace == "" {
			record.Namespace = defaultLinodeNamespace
		}
		records = append(records, record)
	}
	return records, nil
}

func deleteLinodeDownstream(record downstreamOutputRecord, timeout time.Duration) error {
	kubeconfigPath := filepath.Join(fmt.Sprintf("high-availability-%d", record.HAIndex), "kube_config.yaml")
	if _, err := os.Stat(kubeconfigPath); err != nil {
		return fmt.Errorf("kubeconfig not available for HA %d at %s: %w", record.HAIndex, kubeconfigPath, err)
	}

	log.Printf("[downstream][ha-%d] Deleting Linode downstream cluster %s", record.HAIndex, record.ClusterName)
	if err := runKubectlDirect(kubeconfigPath, "delete", "clusters.provisioning.cattle.io", record.ClusterName, "-n", record.Namespace, "--ignore-not-found=true"); err != nil {
		return err
	}
	if err := waitForProvisioningClusterDeleted(kubeconfigPath, record.Namespace, record.ClusterName, timeout); err != nil {
		return err
	}

	if record.MachineConfig != "" {
		if err := runKubectlDirect(kubeconfigPath, "delete", "linodeconfig.rke-machine-config.cattle.io", record.MachineConfig, "-n", record.Namespace, "--ignore-not-found=true"); err != nil {
			log.Printf("[downstream][ha-%d] Warning: failed to delete Linode machine config %s: %v", record.HAIndex, record.MachineConfig, err)
		}
	}
	if record.SecretName != "" {
		if err := runKubectlDirect(kubeconfigPath, "delete", "secret", record.SecretName, "-n", "cattle-global-data", "--ignore-not-found=true"); err != nil {
			log.Printf("[downstream][ha-%d] Warning: failed to delete Linode credential secret %s: %v", record.HAIndex, record.SecretName, err)
		}
	}

	return nil
}

func waitForProvisioningClusterDeleted(kubeconfigPath, namespace, clusterName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, err := getProvisioningClusterStatus(kubeconfigPath, namespace, clusterName)
		if err != nil {
			if strings.Contains(err.Error(), "NotFound") || strings.Contains(err.Error(), "not found") {
				log.Printf("[downstream] Cluster %s deleted", clusterName)
				return nil
			}
			log.Printf("[downstream] Waiting for cluster %s deletion; status check failed: %v", clusterName, err)
		} else {
			log.Printf("[downstream] Waiting for cluster %s deletion", clusterName)
		}
		time.Sleep(20 * time.Second)
	}
	return fmt.Errorf("timed out after %s waiting for downstream cluster %s deletion", timeout, clusterName)
}

func resolveK3SDefaultVersion(rancherURL, bearerToken string) (string, error) {
	if explicit := strings.TrimSpace(os.Getenv("K3S_VERSION")); explicit != "" {
		version := normalizeK3SVersion(explicit)
		log.Printf("[downstream] Using explicit K3s version %s", version)
		return version, nil
	}

	version, err := latestK3SReleaseVersionFromRancherMetadata(rancherURL, bearerToken)
	if err != nil {
		return "", fmt.Errorf("failed to resolve provisionable K3s version: %w", err)
	}
	return version, nil
}

func latestK3SReleaseVersionFromRancherMetadata(rancherURL, bearerToken string) (string, error) {
	rancherURL = strings.TrimRight(clickableURL(rancherURL), "/")
	if strings.TrimSpace(bearerToken) == "" {
		return "", fmt.Errorf("bearer token must not be empty")
	}

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	serverVersion, err := rancherServerVersion(client, rancherURL, bearerToken)
	if err != nil {
		return "", err
	}
	releases, err := k3sReleasesFromRancherMetadataConfig(client, rancherURL, bearerToken, serverVersion)
	if err != nil {
		return "", err
	}
	version, err := selectLatestK3SReleaseVersion(releases, serverVersion)
	if err != nil {
		return "", err
	}
	log.Printf("[downstream] Selected K3s version %s for Rancher server version %s", version, serverVersion)
	return version, nil
}

func k3sReleasesFromRancherMetadataConfig(client *http.Client, rancherURL, bearerToken, serverVersion string) ([]k3sRelease, error) {
	var setting struct {
		Value   string `json:"value"`
		Default string `json:"default"`
	}
	if err := getRancherJSON(client, rancherURL+"/v3/settings/rke-metadata-config", bearerToken, &setting); err != nil {
		return nil, fmt.Errorf("failed to read rke-metadata-config setting: %w", err)
	}

	metadataConfig := strings.TrimSpace(setting.Value)
	if metadataConfig == "" {
		metadataConfig = strings.TrimSpace(setting.Default)
	}
	if metadataConfig == "" {
		return nil, fmt.Errorf("rke-metadata-config setting was empty")
	}

	var config struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(metadataConfig), &config); err != nil {
		return nil, fmt.Errorf("failed to parse rke-metadata-config setting: %w", err)
	}
	if strings.TrimSpace(config.URL) == "" {
		return nil, fmt.Errorf("rke-metadata-config did not include a url")
	}
	log.Printf("[downstream] Reading K3s releases from Rancher KDM metadata %s", config.URL)

	releases, err := fetchK3SReleasesFromMetadataURL(client, config.URL)
	if err != nil {
		return nil, err
	}
	if _, err := selectLatestK3SReleaseVersion(releases, serverVersion); err == nil {
		return releases, nil
	}

	candidateURL := kdmMetadataURLForRancherVersion(serverVersion)
	if candidateURL == "" || candidateURL == config.URL {
		return releases, nil
	}
	log.Printf("[downstream] Rancher KDM metadata %s has no provisionable K3s versions for %s; checking %s", config.URL, serverVersion, candidateURL)
	candidateReleases, err := fetchK3SReleasesFromMetadataURL(client, candidateURL)
	if err != nil {
		return releases, nil
	}
	if _, err := selectLatestK3SReleaseVersion(candidateReleases, serverVersion); err != nil {
		return releases, nil
	}
	return nil, fmt.Errorf("Rancher KDM metadata %s has no provisionable K3s versions for %s, but %s does; Rancher also needs working /v1-k3s-release/releases and /v1-rke2-release/releases endpoints before downstream provisioning can proceed", config.URL, serverVersion, candidateURL)
}

func fetchK3SReleasesFromMetadataURL(client *http.Client, metadataURL string) ([]k3sRelease, error) {
	req, err := http.NewRequest(http.MethodGet, metadataURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch KDM metadata %s: %w", metadataURL, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("KDM metadata %s returned HTTP %d: %s", metadataURL, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var metadata struct {
		K3S struct {
			Releases []k3sRelease `json:"releases"`
		} `json:"k3s"`
	}
	if err := json.Unmarshal(body, &metadata); err != nil {
		return nil, fmt.Errorf("failed to parse KDM metadata %s: %w", metadataURL, err)
	}
	return metadata.K3S.Releases, nil
}

func kdmMetadataURLForRancherVersion(rancherVersion string) string {
	version, err := parseRancherVersion(rancherVersion)
	if err != nil {
		return ""
	}
	segments := version.Segments64()
	if len(segments) < 2 {
		return ""
	}
	return fmt.Sprintf("https://releases.rancher.com/kontainer-driver-metadata/dev-v%d.%d/data.json", segments[0], segments[1])
}

func rancherServerVersion(client *http.Client, rancherURL, bearerToken string) (string, error) {
	var setting struct {
		Value   string `json:"value"`
		Default string `json:"default"`
	}
	if err := getRancherJSON(client, rancherURL+"/v3/settings/server-version", bearerToken, &setting); err != nil {
		return "", fmt.Errorf("failed to read server-version setting: %w", err)
	}

	version := strings.TrimSpace(setting.Value)
	if version == "" {
		version = strings.TrimSpace(setting.Default)
	}
	if version == "" {
		return "", fmt.Errorf("server-version setting was empty")
	}
	return normalizeVersion(version), nil
}

type k3sRelease struct {
	Version                 string                 `json:"version"`
	MinChannelServerVersion string                 `json:"minChannelServerVersion"`
	MaxChannelServerVersion string                 `json:"maxChannelServerVersion"`
	ServerArgs              map[string]interface{} `json:"serverArgs"`
	AgentArgs               map[string]interface{} `json:"agentArgs"`
}

func selectLatestK3SReleaseVersion(releases []k3sRelease, rancherVersion string) (string, error) {
	serverVersion, err := parseRancherVersion(rancherVersion)
	if err != nil {
		return "", err
	}

	var selectedVersion *goversion.Version
	selectedOriginal := ""
	withArgsCount := 0
	compatibleCount := 0

	for _, release := range releases {
		version := normalizeK3SVersion(release.Version)
		if version == "" || release.ServerArgs == nil || release.AgentArgs == nil {
			continue
		}
		withArgsCount++
		if !k3sReleaseSupportsRancherVersion(release, serverVersion) {
			continue
		}
		compatibleCount++
		parsed, err := parseRancherVersion(version)
		if err != nil {
			continue
		}
		if selectedVersion == nil || selectedVersion.LessThan(parsed) || (selectedVersion.Equal(parsed) && version > selectedOriginal) {
			selectedVersion = parsed
			selectedOriginal = version
		}
	}

	if selectedOriginal == "" {
		return "", fmt.Errorf("Rancher K3s release list did not contain any provisionable versions for Rancher %s (total releases=%d, releases with server/agent args=%d, compatible releases=%d)", rancherVersion, len(releases), withArgsCount, compatibleCount)
	}
	return selectedOriginal, nil
}

func k3sReleaseSupportsRancherVersion(release k3sRelease, serverVersion *goversion.Version) bool {
	minVersion, err := parseRancherVersion(release.MinChannelServerVersion)
	if err != nil {
		return false
	}
	maxVersion, err := parseRancherVersion(release.MaxChannelServerVersion)
	if err != nil {
		return false
	}
	return !serverVersion.LessThan(minVersion) && !maxVersion.LessThan(serverVersion)
}

func parseRancherVersion(version string) (*goversion.Version, error) {
	version = strings.TrimSpace(version)
	if version == "" {
		return nil, fmt.Errorf("version must not be empty")
	}
	parsed, err := goversion.NewVersion(strings.TrimPrefix(version, "v"))
	if err != nil {
		return nil, fmt.Errorf("failed to parse version %q: %w", version, err)
	}
	return parsed, nil
}

func normalizeK3SVersion(version string) string {
	return normalizeVersion(version)
}

func normalizeVersion(version string) string {
	version = strings.TrimSpace(version)
	if version == "" || strings.HasPrefix(version, "v") {
		return version
	}
	return "v" + version
}

func createLinodeMachineConfig(rancherURL, bearerToken string, cfg downstreamProvisioningConfig) (string, error) {
	rancherURL = strings.TrimRight(clickableURL(rancherURL), "/")
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	var out struct {
		ID       string `json:"id"`
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	apiURL := fmt.Sprintf("%s/v1/rke-machine-config.cattle.io.linodeconfigs/%s", rancherURL, url.PathEscape(cfg.Namespace))
	if err := postRancherJSON(client, apiURL, bearerToken, linodeMachineConfigPayload(cfg), &out); err != nil {
		return "", err
	}
	if strings.TrimSpace(out.Metadata.Name) != "" {
		return out.Metadata.Name, nil
	}
	if strings.TrimSpace(out.ID) != "" {
		parts := strings.Split(out.ID, "/")
		return parts[len(parts)-1], nil
	}
	return "", fmt.Errorf("Rancher LinodeConfig response did not include a machine config name")
}

func linodeMachineConfigPayload(cfg downstreamProvisioningConfig) map[string]interface{} {
	return map[string]interface{}{
		"image":        cfg.Image,
		"instanceType": cfg.InstanceType,
		"interfaces":   []interface{}{},
		"metadata": map[string]interface{}{
			"annotations":  map[string]string{},
			"generateName": fmt.Sprintf("nc-%s-pool1-", cfg.ClusterName),
			"labels":       map[string]string{},
			"namespace":    cfg.Namespace,
		},
		"region": cfg.Region,
		"type":   "rke-machine-config.cattle.io.linodeconfig",
	}
}

func renderLinodeCredentialSecretManifest(cfg downstreamProvisioningConfig) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: cattle-global-data
  annotations:
    field.cattle.io/name: %s
type: Opaque
stringData:
  linodecredentialConfig-token: %s
`,
		yamlScalar(cfg.SecretName),
		yamlScalar(cfg.SecretName),
		yamlScalar(cfg.LinodeToken),
	)
}

func renderLinodeDownstreamClusterManifest(cfg downstreamProvisioningConfig) string {
	return fmt.Sprintf(`apiVersion: provisioning.cattle.io/v1
kind: Cluster
metadata:
  name: %s
  namespace: %s
spec:
  cloudCredentialSecretName: %s
  kubernetesVersion: %s
  defaultPodSecurityAdmissionConfigurationTemplateName: ""
  localClusterAuthEndpoint:
    enabled: false
  rkeConfig:
    chartValues: {}
    dataDirectories:
      systemAgent: ""
      provisioning: ""
      k8sDistro: ""
    etcd:
      disableSnapshots: false
      s3: null
      snapshotRetention: 5
      snapshotScheduleCron: "0 */5 * * *"
    machineGlobalConfig:
      disable-apiserver: false
      disable-cloud-controller: false
      disable-controller-manager: false
      disable-etcd: false
      disable-kube-proxy: false
      disable-network-policy: false
      disable-scheduler: false
      etcd-expose-metrics: false
      etcd-s3-bucket-lookup-type: auto
      ingress-controller: traefik
      secrets-encryption: false
      secrets-encryption-provider: aescbc
    machineSelectorConfig:
    - config:
        docker: false
        protect-kernel-defaults: false
        selinux: false
    networking: {}
    registries:
      configs: {}
      mirrors: {}
    machinePools:
    - name: pool1
      controlPlaneRole: true
      etcdRole: true
      workerRole: true
      quantity: 1
      drainBeforeDelete: true
      labels: {}
      unhealthyNodeTimeout: "0m"
      machineConfigRef:
        kind: LinodeConfig
        name: %s
    upgradeStrategy:
      controlPlaneConcurrency: "1"
      controlPlaneDrainOptions:
        deleteEmptyDirData: true
        disableEviction: false
        enabled: false
        force: false
        gracePeriod: -1
        ignoreDaemonSets: true
        skipWaitForDeleteTimeoutSeconds: 0
        timeout: 120
      workerConcurrency: "1"
      workerDrainOptions:
        deleteEmptyDirData: true
        disableEviction: false
        enabled: false
        force: false
        gracePeriod: -1
        ignoreDaemonSets: true
        skipWaitForDeleteTimeoutSeconds: 0
        timeout: 120
`,
		yamlScalar(cfg.ClusterName),
		yamlScalar(cfg.Namespace),
		yamlScalar("cattle-global-data:"+cfg.SecretName),
		yamlScalar(cfg.K3SVersion),
		yamlScalar(cfg.MachineName),
	)
}

func waitForProvisioningClusterActive(kubeconfigPath, namespace, clusterName string, timeout time.Duration) error {
	start := time.Now()
	deadline := start.Add(timeout)
	attempt := 0

	for time.Now().Before(deadline) {
		attempt++
		status, err := getProvisioningClusterStatus(kubeconfigPath, namespace, clusterName)
		if err != nil {
			log.Printf("[downstream] Cluster %s status unavailable on attempt %d: %v", clusterName, attempt, err)
		} else {
			summary := summarizeProvisioningClusterStatus(status)
			log.Printf("[downstream] Cluster %s attempt %d after %s: %s", clusterName, attempt, time.Since(start).Round(time.Second), summary)
			if attempt == 1 || attempt%6 == 0 {
				logDownstreamProvisioningDiagnostics(kubeconfigPath, namespace, clusterName, strings.TrimSpace(status.Status.ClusterName))
			}
			if strings.EqualFold(status.Status.Phase, "Active") || status.Status.Ready {
				return nil
			}
		}
		time.Sleep(20 * time.Second)
	}

	return fmt.Errorf("timed out after %s waiting for downstream cluster %s to become active", timeout, clusterName)
}

func logDownstreamProvisioningDiagnostics(kubeconfigPath, namespace, clusterName, managementClusterID string) {
	commands := [][]string{
		{"describe", "clusters.provisioning.cattle.io", clusterName, "-n", namespace},
		{"get", "linodeconfigs.rke-machine-config.cattle.io", "-n", namespace, "-o", "wide"},
		{"get", "clusters.cluster.x-k8s.io", "-A", "-o", "wide"},
		{"describe", "clusters.cluster.x-k8s.io", clusterName, "-n", namespace},
		{"get", "machinedeployments.cluster.x-k8s.io", "-A", "-o", "wide"},
		{"get", "machinesets.cluster.x-k8s.io", "-A", "-o", "wide"},
		{"get", "machines.cluster.x-k8s.io", "-A", "-o", "wide"},
		{"get", "machines.cluster.x-k8s.io", "-A", "-l", "cluster.x-k8s.io/cluster-name=" + clusterName, "-o", "yaml"},
		{"get", "jobs", "-n", namespace, "-o", "wide"},
		{"get", "pods", "-n", namespace, "-o", "wide"},
		{"get", "events", "-n", namespace, "--sort-by=.lastTimestamp"},
	}
	if managementClusterID != "" {
		commands = append(commands,
			[]string{"get", "clusters.management.cattle.io", managementClusterID, "-o", "yaml"},
		)
	}

	for _, args := range commands {
		output, err := runKubectlOutput(kubeconfigPath, args...)
		label := strings.Join(args, " ")
		if err != nil {
			log.Printf("[downstream][diagnostics][%s] %v", label, err)
			continue
		}
		log.Printf("[downstream][diagnostics][%s]\n%s", label, trimDiagnosticOutput(output))
	}
	logDownstreamMachinePodDiagnostics(kubeconfigPath, namespace, clusterName)
}

func logDownstreamMachinePodDiagnostics(kubeconfigPath, namespace, clusterName string) {
	output, err := runKubectlOutput(kubeconfigPath, "get", "pods", "-n", namespace, "-o", "json")
	if err != nil {
		log.Printf("[downstream][diagnostics][get pods -n %s -o json] %v", namespace, err)
		return
	}

	var pods podList
	if err := json.Unmarshal([]byte(output), &pods); err != nil {
		log.Printf("[downstream][diagnostics][get pods -n %s -o json] parse failed: %v", namespace, err)
		return
	}

	logged := 0
	for _, pod := range pods.Items {
		podName := strings.TrimSpace(pod.Metadata.Name)
		if podName == "" || !strings.Contains(podName, clusterName) {
			continue
		}
		logged++
		logDownstreamDiagnosticCommand(kubeconfigPath, "describe", "pod", podName, "-n", namespace)
		logDownstreamDiagnosticCommand(kubeconfigPath, "logs", podName, "-n", namespace, "--all-containers=true", "--tail=200")
		if logged >= 5 {
			log.Printf("[downstream][diagnostics] skipping remaining machine pod logs after %d pods", logged)
			return
		}
	}
	if logged == 0 {
		log.Printf("[downstream][diagnostics] no machine pods matched cluster %s in namespace %s", clusterName, namespace)
	}
}

func logDownstreamDiagnosticCommand(kubeconfigPath string, args ...string) {
	output, err := runKubectlOutput(kubeconfigPath, args...)
	label := strings.Join(args, " ")
	if err != nil {
		log.Printf("[downstream][diagnostics][%s] %v", label, err)
		return
	}
	log.Printf("[downstream][diagnostics][%s]\n%s", label, trimDiagnosticOutput(output))
}

func trimDiagnosticOutput(output string) string {
	const maxLen = 6000
	output = strings.TrimSpace(output)
	if len(output) <= maxLen {
		return output
	}
	return output[:maxLen] + "\n...<truncated>"
}

func getProvisioningClusterStatus(kubeconfigPath, namespace, clusterName string) (provisioningClusterStatus, error) {
	output, err := runKubectlOutput(kubeconfigPath, "get", "clusters.provisioning.cattle.io", clusterName, "-n", namespace, "-o", "json")
	if err != nil {
		return provisioningClusterStatus{}, err
	}

	var status provisioningClusterStatus
	if err := json.Unmarshal([]byte(output), &status); err != nil {
		return provisioningClusterStatus{}, fmt.Errorf("failed to parse provisioning cluster status: %w", err)
	}
	return status, nil
}

func summarizeProvisioningClusterStatus(status provisioningClusterStatus) string {
	parts := []string{fmt.Sprintf("phase=%s ready=%t cluster=%s", status.Status.Phase, status.Status.Ready, status.Status.ClusterName)}
	for _, condition := range status.Status.Conditions {
		if condition.Status == "" || condition.Type == "" {
			continue
		}
		detail := fmt.Sprintf("%s=%s", condition.Type, condition.Status)
		if condition.Reason != "" {
			detail += "/" + condition.Reason
		}
		if condition.Message != "" {
			detail += " " + condition.Message
		}
		parts = append(parts, detail)
	}
	return strings.Join(parts, "; ")
}

func writeDownstreamOutputs(instanceNum int, cfg downstreamProvisioningConfig, haOutputs TerraformOutputs, managementClusterID string) error {
	outputDir := automationOutputDir()
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return err
	}

	envPath := filepath.Join(outputDir, fmt.Sprintf("downstream-ha-%d.env", instanceNum))
	adminToken, err := createRancherAdminToken(haOutputs.RancherURL, viper.GetString("rancher.bootstrap_password"))
	if err != nil {
		return err
	}
	if err := configureRancherServerURL(haOutputs.RancherURL, adminToken); err != nil {
		return err
	}
	envContent := fmt.Sprintf("RANCHER_HOST=%s\nRANCHER_ADMIN_TOKEN=%s\nCLUSTER_NAME=%s\n", rancherTestsHost(haOutputs.RancherURL), adminToken, cfg.ClusterName)
	if err := os.WriteFile(envPath, []byte(envContent), 0o600); err != nil {
		return err
	}

	jsonPath := filepath.Join(outputDir, fmt.Sprintf("downstream-ha-%d.json", instanceNum))
	payload := map[string]string{
		"rancher_host":          clickableURL(haOutputs.RancherURL),
		"cluster_name":          cfg.ClusterName,
		"management_cluster_id": managementClusterID,
		"kubeconfig_path":       downstreamKubeconfigPath(instanceNum),
		"secret_name":           cfg.SecretName,
		"namespace":             cfg.Namespace,
		"k3s_version":           cfg.K3SVersion,
		"linode_region":         cfg.Region,
		"linode_type":           cfg.InstanceType,
		"linode_image":          cfg.Image,
		"machine_config":        cfg.MachineName,
	}
	payloadWithIndex := map[string]interface{}{}
	for key, value := range payload {
		payloadWithIndex[key] = value
	}
	payloadWithIndex["ha_index"] = instanceNum
	data, err := json.MarshalIndent(payloadWithIndex, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(jsonPath, append(data, '\n'), 0o600)
}

func downstreamKubeconfigPath(instanceNum int) string {
	return automationOutputPath(fmt.Sprintf("downstream-ha-%d.kubeconfig", instanceNum))
}

func writeDownstreamKubeconfig(instanceNum int, cfg downstreamProvisioningConfig, haOutputs TerraformOutputs, managementClusterID string) (string, error) {
	if err := os.MkdirAll(automationOutputDir(), 0o755); err != nil {
		return "", err
	}
	kubeconfigPath := downstreamKubeconfigPath(instanceNum)
	adminToken, err := createRancherAdminToken(haOutputs.RancherURL, viper.GetString("rancher.bootstrap_password"))
	if err != nil {
		return "", err
	}
	kubeconfig, err := generateRancherKubeconfig(haOutputs.RancherURL, adminToken, managementClusterID)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(kubeconfigPath, []byte(kubeconfig), 0o600); err != nil {
		return "", err
	}
	log.Printf("[downstream][ha-%d] Wrote downstream kubeconfig for %s (%s)", instanceNum, cfg.ClusterName, managementClusterID)
	return kubeconfigPath, nil
}

func kubectlApply(kubeconfigPath, manifest string) error {
	cmd := exec.Command("kubectl", "--kubeconfig", kubeconfigPath, "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl apply failed: %w", err)
	}
	return nil
}

func yamlScalar(value string) string {
	return strconv.Quote(value)
}

func envOrDefaultTrimmed(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func randomHex(byteCount int) string {
	buf := make([]byte, byteCount)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func dnsLabel(value string) string {
	value = strings.ToLower(value)
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if valid {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	result := strings.Trim(b.String(), "-")
	if len(result) > 53 {
		result = strings.Trim(result[:53], "-")
	}
	if result == "" {
		return "downstream"
	}
	return result
}

func shortRunID(runID string) string {
	runID = strings.TrimSpace(runID)
	if len(runID) <= 8 {
		return runID
	}
	return runID[len(runID)-8:]
}

func downstreamClusterNamePrefix(explicitPrefix, runID string) string {
	if explicitPrefix = strings.TrimSpace(explicitPrefix); explicitPrefix != "" {
		return explicitPrefix
	}
	if strings.TrimSpace(runID) != "" {
		return "gha"
	}
	return "ha-rancher-rke2"
}
