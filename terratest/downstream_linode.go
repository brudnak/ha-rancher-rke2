package test

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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
	RootPassword string
	Tags         string
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
	if namePrefix == "" {
		namePrefix = "ha-rancher-rke2"
	}

	timeout := durationFromEnv("LINODE_DOWNSTREAM_TIMEOUT", 45*time.Minute)
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

	k3sVersion, err := resolveK3SDefaultVersion(kubeconfigPath)
	if err != nil {
		return err
	}

	suffix := randomHex(4)
	clusterName := dnsLabel(fmt.Sprintf("%s-ha%d-%s", namePrefix, instanceNum, suffix))
	if runID != "" {
		clusterName = dnsLabel(fmt.Sprintf("%s-%s-ha%d-%s", namePrefix, shortRunID(runID), instanceNum, suffix))
	}

	rootPassword := randomRootPassword()
	maskGitHubActionsValue(rootPassword)
	cfg := downstreamProvisioningConfig{
		ClusterName:  clusterName,
		MachineName:  dnsLabel(clusterName + "-pool1"),
		SecretName:   dnsLabel("cc-" + clusterName),
		Namespace:    defaultLinodeNamespace,
		Region:       envOrDefaultTrimmed("LINODE_REGION", defaultLinodeRegion),
		InstanceType: envOrDefaultTrimmed("LINODE_INSTANCE_TYPE", defaultLinodeInstanceType),
		Image:        envOrDefaultTrimmed("LINODE_IMAGE", defaultLinodeImage),
		K3SVersion:   k3sVersion,
		RootPassword: rootPassword,
		Tags:         fmt.Sprintf("ha-rancher-rke2,%s,%s", namePrefix, clusterName),
		LinodeToken:  linodeToken,
	}

	log.Printf("[downstream][ha-%d] Creating one-node Linode K3s cluster %s on %s (%s, %s, %s)",
		instanceNum, cfg.ClusterName, clickableURL(haOutputs.RancherURL), cfg.K3SVersion, cfg.Region, cfg.InstanceType)

	if err := kubectlApply(kubeconfigPath, renderLinodeDownstreamManifests(cfg)); err != nil {
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
		t.Skip("no automation-output/downstream-ha-*.json files found; skipping Linode downstream cleanup")
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
	paths, err := filepath.Glob(filepath.Join("automation-output", "downstream-ha-*.json"))
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

func resolveK3SDefaultVersion(kubeconfigPath string) (string, error) {
	if explicit := strings.TrimSpace(os.Getenv("K3S_VERSION")); explicit != "" {
		return explicit, nil
	}

	output, err := runKubectlOutput(kubeconfigPath, "get", "settings.management.cattle.io", "k3s-default-version", "-o", "json")
	if err != nil {
		return "", fmt.Errorf("failed to read Rancher k3s-default-version setting: %w", err)
	}

	var setting struct {
		Value       string `json:"value"`
		Default     string `json:"default"`
		DefaultBool string `json:"defaultBool"`
	}
	if err := json.Unmarshal([]byte(output), &setting); err != nil {
		return "", fmt.Errorf("failed to parse k3s-default-version setting: %w", err)
	}
	if strings.TrimSpace(setting.Value) != "" {
		return strings.TrimSpace(setting.Value), nil
	}
	if strings.TrimSpace(setting.Default) != "" {
		return strings.TrimSpace(setting.Default), nil
	}
	return "", fmt.Errorf("k3s-default-version setting was empty; set K3S_VERSION explicitly")
}

func renderLinodeDownstreamManifests(cfg downstreamProvisioningConfig) string {
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
---
apiVersion: rke-machine-config.cattle.io/v1
kind: LinodeConfig
metadata:
  name: %s
  namespace: %s
authorizedUsers: ""
createPrivateIp: false
dockerPort: "2376"
image: %s
instanceType: %s
region: %s
rootPass: %s
sshPort: "22"
sshUser: root
stackscript: ""
stackscriptData: ""
swapSize: "512"
tags: %s
token: ""
type: rke-machine-config.cattle.io.linodeconfig
uaPrefix: Rancher
---
apiVersion: provisioning.cattle.io/v1
kind: Cluster
metadata:
  name: %s
  namespace: %s
spec:
  cloudCredentialSecretName: %s
  kubernetesVersion: %s
  localClusterAuthEndpoint:
    enabled: false
  rkeConfig:
    etcd:
      snapshotRetention: 5
      snapshotScheduleCron: "0 */5 * * *"
    machineGlobalConfig:
      cni: calico
      disable-kube-proxy: false
      etcd-expose-metrics: false
      ingress-controller: traefik
      profile: null
    machinePools:
    - name: pool1
      controlPlaneRole: true
      etcdRole: true
      workerRole: true
      quantity: 1
      drainBeforeDelete: true
      machineConfigRef:
        apiVersion: rke-machine-config.cattle.io/v1
        kind: LinodeConfig
        name: %s
    upgradeStrategy:
      controlPlaneConcurrency: "10%%"
      controlPlaneDrainOptions: {}
      workerConcurrency: "10%%"
      workerDrainOptions: {}
`,
		yamlScalar(cfg.SecretName),
		yamlScalar(cfg.SecretName),
		yamlScalar(cfg.LinodeToken),
		yamlScalar(cfg.MachineName),
		yamlScalar(cfg.Namespace),
		yamlScalar(cfg.Image),
		yamlScalar(cfg.InstanceType),
		yamlScalar(cfg.Region),
		yamlScalar(cfg.RootPassword),
		yamlScalar(cfg.Tags),
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
			if strings.EqualFold(status.Status.Phase, "Active") || status.Status.Ready {
				return nil
			}
		}
		time.Sleep(20 * time.Second)
	}

	return fmt.Errorf("timed out after %s waiting for downstream cluster %s to become active", timeout, clusterName)
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
	outputDir := "automation-output"
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return err
	}

	envPath := filepath.Join(outputDir, fmt.Sprintf("downstream-ha-%d.env", instanceNum))
	adminToken, err := createRancherAdminToken(haOutputs.RancherURL, viper.GetString("rancher.bootstrap_password"))
	if err != nil {
		return err
	}
	envContent := fmt.Sprintf("RANCHER_HOST=%s\nRANCHER_ADMIN_TOKEN=%s\nCLUSTER_NAME=%s\n", clickableURL(haOutputs.RancherURL), adminToken, cfg.ClusterName)
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
	return filepath.Join("automation-output", fmt.Sprintf("downstream-ha-%d.kubeconfig", instanceNum))
}

func writeDownstreamKubeconfig(instanceNum int, cfg downstreamProvisioningConfig, haOutputs TerraformOutputs, managementClusterID string) (string, error) {
	if err := os.MkdirAll("automation-output", 0o755); err != nil {
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

func randomRootPassword() string {
	return "Rancher-" + randomHex(16) + "aA1!"
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
