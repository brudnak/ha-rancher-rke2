package test

import (
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

func TestHAUpgradeRancher(t *testing.T) {
	requireExplicitLifecycleTest(t, "TestHAUpgradeRancher")
	setupConfig(t)

	upgradeVersion := normalizeVersionInput(os.Getenv("RANCHER_UPGRADE_VERSION"))
	if upgradeVersion == "" {
		t.Skip("RANCHER_UPGRADE_VERSION is not set; skipping Rancher upgrade")
	}

	totalHAs := viper.GetInt("total_has")
	if totalHAs < 1 {
		t.Fatal("total_has must be at least 1")
	}

	if totalHAs == 1 {
		viper.Set("rancher.version", upgradeVersion)
		viper.Set("rancher.versions", []string{})
	} else {
		upgradeVersions := make([]string, totalHAs)
		for i := range upgradeVersions {
			upgradeVersions[i] = upgradeVersion
		}
		viper.Set("rancher.versions", upgradeVersions)
	}
	upgradePlans, err := resolveAutoRancherPlans(totalHAs)
	if err != nil {
		t.Fatalf("failed to resolve Rancher upgrade plan for %s: %v", upgradeVersion, err)
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
		plan := upgradePlans[i-1]
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := upgradeHAInstanceRancher(instanceNum, outputs, plan); err != nil {
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
		t.Fatalf("Rancher upgrade failed:\n%s", strings.Join(failures, "\n"))
	}

	timeout := durationFromEnv("RANCHER_UPGRADE_READY_TIMEOUT", durationFromEnv("RANCHER_READY_TIMEOUT", 30*time.Minute))
	initialDelay := durationFromEnv("RANCHER_UPGRADE_READY_INITIAL_DELAY", 45*time.Second)
	settleDelay := durationFromEnv("RANCHER_UPGRADE_READY_SETTLE_DELAY", durationFromEnv("RANCHER_READY_SETTLE_DELAY", 60*time.Second))

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
		t.Fatalf("Rancher upgrade readiness failed:\n%s", strings.Join(failures, "\n"))
	}
}

func upgradeHAInstanceRancher(instanceNum int, outputs map[string]string, plan *RancherResolvedPlan) error {
	haOutputs := getHAOutputs(instanceNum, outputs)
	haDir := fmt.Sprintf("high-availability-%d", instanceNum)
	currentDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get current directory: %w", err)
	}

	absHADir := filepath.Join(currentDir, haDir)
	absKubeConfigPath := filepath.Join(absHADir, "kube_config.yaml")
	if _, err := os.Stat(absKubeConfigPath); err != nil {
		return fmt.Errorf("kubeconfig not available for HA %d at %s: %w", instanceNum, absKubeConfigPath, err)
	}

	helmCommand := buildAutoHelmCommand(
		rancherHelmOperationUpgrade,
		plan.ChartRepoAlias,
		plan.ChartVersion,
		viper.GetString("rancher.bootstrap_password"),
		plan.RancherImage,
		plan.RancherImageTag,
		plan.AgentImage,
	)
	helmCommand = rancherHelmCommandForHA(helmCommand, haOutputs.RancherURL)

	log.Printf("[upgrade][ha-%d] Upgrading Rancher at %s to requested version %s using %s/rancher@%s",
		instanceNum, clickableURL(haOutputs.RancherURL), plan.RequestedVersion, plan.ChartRepoAlias, plan.ChartVersion)
	for _, explanation := range plan.Explanation {
		log.Printf("[upgrade][ha-%d] %s", instanceNum, explanation)
	}

	cmd := exec.Command("bash", "-lc", helmCommand)
	cmd.Dir = absHADir
	cmd.Env = append(os.Environ(), fmt.Sprintf("KUBECONFIG=%s", absKubeConfigPath))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to run Rancher upgrade for HA %d: %w", instanceNum, err)
	}

	log.Printf("[upgrade][ha-%d] Helm upgrade completed; waiting for Rancher readiness next", instanceNum)
	return nil
}
