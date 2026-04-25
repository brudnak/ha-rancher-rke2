package test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/viper"
)

func TestHAWriteLocalSuiteEnv(t *testing.T) {
	requireExplicitLifecycleTest(t, "TestHAWriteLocalSuiteEnv")
	setupConfig(t)

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
		haOutputs := getHAOutputs(instanceNum, outputs)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := writeLocalSuiteEnv(instanceNum, haOutputs); err != nil {
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
		t.Fatalf("local suite env export failed:\n%s", strings.Join(failures, "\n"))
	}
}

func writeLocalSuiteEnv(instanceNum int, haOutputs TerraformOutputs) error {
	adminToken, err := createRancherAdminToken(haOutputs.RancherURL, viper.GetString("rancher.bootstrap_password"))
	if err != nil {
		return err
	}

	return writeSuiteEnvOutput(instanceNum, "local", haOutputs, adminToken, "local-suite")
}

func writeSuiteEnvOutput(instanceNum int, clusterName string, haOutputs TerraformOutputs, adminToken, name string) error {
	outputDir := "automation-output"
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return err
	}

	envPath := filepath.Join(outputDir, fmt.Sprintf("%s-ha-%d.env", name, instanceNum))
	envContent := fmt.Sprintf("RANCHER_HOST=%s\nRANCHER_ADMIN_TOKEN=%s\nCLUSTER_NAME=%s\n",
		clickableURL(haOutputs.RancherURL), adminToken, clusterName)
	if err := os.WriteFile(envPath, []byte(envContent), 0o600); err != nil {
		return err
	}

	jsonPath := filepath.Join(outputDir, fmt.Sprintf("%s-ha-%d.json", name, instanceNum))
	payload := map[string]interface{}{
		"ha_index":     instanceNum,
		"rancher_host": clickableURL(haOutputs.RancherURL),
		"cluster_name": clusterName,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(jsonPath, append(data, '\n'), 0o600)
}
