package test

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/viper"
)

type webhookDeploymentList struct {
	Items []struct {
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Name  string `json:"name"`
						Image string `json:"image"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	} `json:"items"`
}

type webhookDeploymentTarget struct {
	Namespace      string
	DeploymentName string
	ContainerName  string
	CurrentImage   string
}

func TestHAOverrideLocalWebhook(t *testing.T) {
	requireExplicitLifecycleTest(t, "TestHAOverrideLocalWebhook")
	setupConfig(t)

	webhookImage := strings.TrimSpace(os.Getenv("RANCHER_WEBHOOK_IMAGE"))
	if webhookImage == "" {
		t.Skip("RANCHER_WEBHOOK_IMAGE is not set; skipping local webhook override")
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

	var wg sync.WaitGroup
	errCh := make(chan error, totalHAs)
	for i := 1; i <= totalHAs; i++ {
		instanceNum := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := overrideLocalWebhook(instanceNum, webhookImage); err != nil {
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
		t.Fatalf("local webhook override failed:\n%s", strings.Join(failures, "\n"))
	}

	timeout := durationFromEnv("RANCHER_WEBHOOK_READY_TIMEOUT", durationFromEnv("RANCHER_READY_TIMEOUT", 20*time.Minute))
	initialDelay := durationFromEnv("RANCHER_WEBHOOK_READY_INITIAL_DELAY", 30*time.Second)
	settleDelay := durationFromEnv("RANCHER_WEBHOOK_READY_SETTLE_DELAY", durationFromEnv("RANCHER_READY_SETTLE_DELAY", 45*time.Second))

	errCh = make(chan error, totalHAs)
	for i := 1; i <= totalHAs; i++ {
		instanceNum := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := waitForHAReady(instanceNum, outputs, timeout, initialDelay, settleDelay); err != nil {
				errCh <- err
			}
		}()
	}

	wg.Wait()
	close(errCh)

	failures = failures[:0]
	for err := range errCh {
		failures = append(failures, err.Error())
	}
	if len(failures) > 0 {
		t.Fatalf("local webhook override readiness failed:\n%s", strings.Join(failures, "\n"))
	}
}

func TestHAOverrideDownstreamWebhook(t *testing.T) {
	requireExplicitLifecycleTest(t, "TestHAOverrideDownstreamWebhook")
	setupConfig(t)

	webhookImage := strings.TrimSpace(os.Getenv("RANCHER_WEBHOOK_IMAGE"))
	if webhookImage == "" {
		t.Skip("RANCHER_WEBHOOK_IMAGE is not set; skipping downstream webhook override")
	}

	records, err := readDownstreamOutputRecords()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) == 0 {
		t.Skip("no downstream-ha-*.json files found; skipping downstream webhook override")
	}

	var wg sync.WaitGroup
	errCh := make(chan error, len(records))
	for _, record := range records {
		record := record
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := overrideDownstreamWebhook(record, webhookImage); err != nil {
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
		t.Fatalf("downstream webhook override failed:\n%s", strings.Join(failures, "\n"))
	}
}

func TestHAWaitWebhookChartVersion(t *testing.T) {
	requireExplicitLifecycleTest(t, "TestHAWaitWebhookChartVersion")
	setupConfig(t)

	expectedVersion, err := expectedWebhookChartVersion()
	if err != nil {
		t.Fatal(err)
	}

	records, err := readDownstreamOutputRecords()
	if err != nil {
		t.Fatal(err)
	}

	totalHAs := viper.GetInt("total_has")
	if totalHAs < 1 {
		t.Fatal("total_has must be at least 1")
	}

	timeout := durationFromEnv("RANCHER_DOWNSTREAM_WEBHOOK_CHART_TIMEOUT", 15*time.Minute)
	interval := durationFromEnv("RANCHER_DOWNSTREAM_WEBHOOK_CHART_INTERVAL", 20*time.Second)
	settleDelay := durationFromEnv("RANCHER_DOWNSTREAM_WEBHOOK_CHART_SETTLE_DELAY", 30*time.Second)

	var wg sync.WaitGroup
	errCh := make(chan error, totalHAs+len(records))
	for i := 1; i <= totalHAs; i++ {
		instanceNum := i
		kubeconfigPath := filepath.Join(fmt.Sprintf("high-availability-%d", instanceNum), "kube_config.yaml")
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := waitForWebhookChartVersion(instanceNum, "local", "local", kubeconfigPath, expectedVersion, timeout, interval, settleDelay); err != nil {
				errCh <- err
			}
		}()
	}
	for _, record := range records {
		record := record
		wg.Add(1)
		go func() {
			defer wg.Done()
			kubeconfigPath, err := ensureDownstreamKubeconfig(record)
			if err != nil {
				errCh <- err
				return
			}
			if err := waitForWebhookChartVersion(record.HAIndex, "downstream", record.ClusterName, kubeconfigPath, expectedVersion, timeout, interval, settleDelay); err != nil {
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
		t.Fatalf("webhook chart version wait failed:\n%s", strings.Join(failures, "\n"))
	}
}

func overrideLocalWebhook(instanceNum int, webhookImage string) error {
	haDir := fmt.Sprintf("high-availability-%d", instanceNum)
	currentDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get current directory: %w", err)
	}

	kubeconfigPath := filepath.Join(currentDir, haDir, "kube_config.yaml")
	if _, err := os.Stat(kubeconfigPath); err != nil {
		return fmt.Errorf("kubeconfig not available for HA %d at %s: %w", instanceNum, kubeconfigPath, err)
	}

	target, err := overrideWebhookDeployment(instanceNum, "local", "local", kubeconfigPath, "cattle-system", webhookImage)
	if err != nil {
		return err
	}
	return writeWebhookOverrideRecord("local", instanceNum, "local", target, webhookImage)
}

func overrideDownstreamWebhook(record downstreamOutputRecord, webhookImage string) error {
	kubeconfigPath, err := ensureDownstreamKubeconfig(record)
	if err != nil {
		return err
	}
	target, err := overrideWebhookDeployment(record.HAIndex, "downstream", record.ClusterName, kubeconfigPath, "", webhookImage)
	if err != nil {
		return err
	}
	return writeWebhookOverrideRecord("downstream", record.HAIndex, record.ClusterName, target, webhookImage)
}

func expectedWebhookChartVersion() (string, error) {
	if version := strings.TrimSpace(os.Getenv("RANCHER_WEBHOOK_CHART_VERSION")); version != "" {
		return version, nil
	}

	data, err := os.ReadFile("signoff-plan.json")
	if err != nil {
		return "", fmt.Errorf("failed to read signoff-plan.json for target webhook version: %w", err)
	}
	var plan struct {
		TargetWebhookBuild string `json:"target_webhook_build"`
	}
	if err := json.Unmarshal(data, &plan); err != nil {
		return "", fmt.Errorf("failed to parse signoff-plan.json for target webhook version: %w", err)
	}
	if strings.TrimSpace(plan.TargetWebhookBuild) == "" {
		return "", fmt.Errorf("signoff-plan.json target_webhook_build is empty")
	}
	return strings.TrimSpace(plan.TargetWebhookBuild), nil
}

func waitForWebhookChartVersion(instanceNum int, scope, clusterName, kubeconfigPath, expectedVersion string, timeout, interval, settleDelay time.Duration) error {
	start := time.Now()
	deadline := start.Add(timeout)
	attempt := 0
	for time.Now().Before(deadline) {
		attempt++
		version, err := installedWebhookChartVersion(kubeconfigPath)
		if err != nil {
			log.Printf("[webhook][ha-%d][%s:%s] Attempt %d after %s: chart version unavailable: %v",
				instanceNum, scope, clusterName, attempt, time.Since(start).Round(time.Second), err)
		} else {
			log.Printf("[webhook][ha-%d][%s:%s] Attempt %d after %s: webhook chart=%s want=%s",
				instanceNum, scope, clusterName, attempt, time.Since(start).Round(time.Second), version, expectedVersion)
			if version == expectedVersion {
				target, err := discoverWebhookDeployment(kubeconfigPath, "")
				if err != nil {
					return err
				}
				if err := runKubectlDirect(kubeconfigPath, "rollout", "status", "deployment/"+target.DeploymentName, "-n", target.Namespace, "--timeout=5m"); err != nil {
					return err
				}
				if settleDelay > 0 {
					log.Printf("[webhook][ha-%d][%s:%s] Webhook chart version is ready; settling for %s",
						instanceNum, scope, clusterName, settleDelay)
					time.Sleep(settleDelay)
				}
				return nil
			}
		}
		time.Sleep(interval)
	}

	return fmt.Errorf("timed out after %s waiting for %s cluster %s webhook chart version %s", timeout, scope, clusterName, expectedVersion)
}

func installedWebhookChartVersion(kubeconfigPath string) (string, error) {
	output, err := runKubectlOutput(kubeconfigPath, "get", "apps.catalog.cattle.io", "rancher-webhook", "-n", "cattle-system", "-o", "json")
	if err != nil {
		return "", err
	}
	var app struct {
		Spec struct {
			Chart struct {
				Metadata struct {
					Version string `json:"version"`
				} `json:"metadata"`
			} `json:"chart"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte(output), &app); err != nil {
		return "", fmt.Errorf("failed to parse rancher-webhook app: %w", err)
	}
	if strings.TrimSpace(app.Spec.Chart.Metadata.Version) == "" {
		return "", fmt.Errorf("rancher-webhook app spec.chart.metadata.version is empty")
	}
	return strings.TrimSpace(app.Spec.Chart.Metadata.Version), nil
}

func ensureDownstreamKubeconfig(record downstreamOutputRecord) (string, error) {
	kubeconfigPath := strings.TrimSpace(record.KubeconfigPath)
	if kubeconfigPath == "" {
		kubeconfigPath = downstreamKubeconfigPath(record.HAIndex)
	}
	if _, err := os.Stat(kubeconfigPath); err == nil {
		return kubeconfigPath, nil
	}
	if record.ManagementClusterID == "" {
		return "", fmt.Errorf("downstream kubeconfig missing for HA %d and management_cluster_id is empty; rerun TestHAProvisionLinodeDownstream", record.HAIndex)
	}
	adminToken, err := createRancherAdminToken(record.RancherHost, viper.GetString("rancher.bootstrap_password"))
	if err != nil {
		return "", err
	}
	kubeconfig, err := generateRancherKubeconfig(record.RancherHost, adminToken, record.ManagementClusterID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(kubeconfigPath), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(kubeconfigPath, []byte(kubeconfig), 0o600); err != nil {
		return "", err
	}
	return kubeconfigPath, nil
}

func overrideWebhookDeployment(instanceNum int, scope, clusterName, kubeconfigPath, namespace, webhookImage string) (webhookDeploymentTarget, error) {
	target, err := discoverWebhookDeployment(kubeconfigPath, namespace)
	if err != nil {
		return webhookDeploymentTarget{}, err
	}
	if target.Namespace == "" {
		target.Namespace = namespace
	}
	if target.Namespace == "" {
		return webhookDeploymentTarget{}, fmt.Errorf("webhook deployment namespace was empty for %s cluster %s", scope, clusterName)
	}

	log.Printf("[webhook][ha-%d][%s:%s] Overriding %s/%s container %s image from %s to %s",
		instanceNum, scope, clusterName, target.Namespace, target.DeploymentName, target.ContainerName, target.CurrentImage, webhookImage)
	if err := runKubectlDirect(kubeconfigPath, "set", "image", "deployment/"+target.DeploymentName, target.ContainerName+"="+webhookImage, "-n", target.Namespace); err != nil {
		return webhookDeploymentTarget{}, err
	}
	if err := runKubectlDirect(kubeconfigPath, "rollout", "status", "deployment/"+target.DeploymentName, "-n", target.Namespace, "--timeout=10m"); err != nil {
		return webhookDeploymentTarget{}, err
	}

	log.Printf("[webhook][ha-%d][%s:%s] rancher-webhook rollout completed", instanceNum, scope, clusterName)
	return target, nil
}

func discoverLocalWebhookDeployment(kubeconfigPath string) (webhookDeploymentTarget, error) {
	return discoverWebhookDeployment(kubeconfigPath, "cattle-system")
}

func discoverWebhookDeployment(kubeconfigPath, namespace string) (webhookDeploymentTarget, error) {
	args := []string{"get", "deployments", "-o", "json"}
	if namespace == "" {
		args = []string{"get", "deployments", "-A", "-o", "json"}
	} else {
		args = []string{"get", "deployments", "-n", namespace, "-o", "json"}
	}
	output, err := runKubectlOutput(kubeconfigPath, args...)
	if err != nil {
		return webhookDeploymentTarget{}, err
	}

	return selectWebhookDeployment([]byte(output), namespace)
}

func selectLocalWebhookDeployment(data []byte) (webhookDeploymentTarget, error) {
	return selectWebhookDeployment(data, "cattle-system")
}

func selectWebhookDeployment(data []byte, fallbackNamespace string) (webhookDeploymentTarget, error) {
	var deployments webhookDeploymentList
	if err := json.Unmarshal(data, &deployments); err != nil {
		return webhookDeploymentTarget{}, fmt.Errorf("failed to parse deployments: %w", err)
	}
	for _, deployment := range deployments.Items {
		for _, container := range deployment.Spec.Template.Spec.Containers {
			deploymentName := strings.ToLower(deployment.Metadata.Name)
			containerName := strings.ToLower(container.Name)
			image := strings.ToLower(container.Image)
			if strings.Contains(deploymentName, "rancher-webhook") ||
				strings.Contains(containerName, "rancher-webhook") ||
				strings.Contains(image, "rancher-webhook") {
				namespace := deployment.Metadata.Namespace
				if namespace == "" {
					namespace = fallbackNamespace
				}
				return webhookDeploymentTarget{
					Namespace:      namespace,
					DeploymentName: deployment.Metadata.Name,
					ContainerName:  container.Name,
					CurrentImage:   container.Image,
				}, nil
			}
		}
	}

	return webhookDeploymentTarget{}, fmt.Errorf("could not find a deployment/container/image matching rancher-webhook")
}

func writeWebhookOverrideRecord(scope string, instanceNum int, clusterName string, target webhookDeploymentTarget, webhookImage string) error {
	if err := os.MkdirAll(automationOutputDir(), 0o755); err != nil {
		return err
	}
	payload := map[string]interface{}{
		"scope":            scope,
		"ha_index":         instanceNum,
		"cluster_name":     clusterName,
		"namespace":        target.Namespace,
		"deployment":       target.DeploymentName,
		"container":        target.ContainerName,
		"previous_image":   target.CurrentImage,
		"candidate_image":  webhookImage,
		"rollout_complete": true,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	path := automationOutputPath(fmt.Sprintf("webhook-override-%s-ha-%d.json", scope, instanceNum))
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func runKubectlDirect(kubeconfigPath string, args ...string) error {
	cmd := exec.Command("kubectl", append([]string{"--kubeconfig", kubeconfigPath}, args...)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl %s failed: %w", strings.Join(args, " "), err)
	}
	return nil
}

func runKubectlOutput(kubeconfigPath string, args ...string) (string, error) {
	cmd := exec.Command("kubectl", append([]string{"--kubeconfig", kubeconfigPath}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("kubectl %s failed: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}
