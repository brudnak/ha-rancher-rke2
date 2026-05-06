package test

import (
	"fmt"
	"log"
	"sync"
	"testing"

	"github.com/brudnak/ha-rancher-rke2/terratest/settings"
	"github.com/gruntwork-io/terratest/modules/terraform"
	"github.com/spf13/viper"
)

func TestHaSetup(t *testing.T) {
	requireExplicitLifecycleTest(t, "TestHaSetup")
	setupConfig(t)

	resolvedPlans, err := resolveRancherSetup()
	if err != nil {
		t.Fatalf("Rancher setup canceled or failed: %v", err)
	}

	totalHAs := viper.GetInt("total_has")
	if totalHAs < 1 {
		t.Fatal("total_has must be at least 1")
	}
	if err := settings.ValidateAWSPrefixConfig(); err != nil {
		t.Fatalf("AWS prefix preflight failed: %v", err)
	}
	if err := settings.ValidateAWSPemKeyNameConfig(); err != nil {
		t.Fatalf("AWS PEM key preflight failed: %v", err)
	}
	if err := settings.ValidateCustomHostnameConfig(totalHAs); err != nil {
		t.Fatalf("Custom Rancher URL preflight failed: %v", err)
	}

	helmCommands := viper.GetStringSlice("rancher.helm_commands")
	if len(helmCommands) != totalHAs {
		t.Fatalf("Number of Helm commands (%d) does not match the number of HA instances (%d). Please ensure you have exactly %d Helm commands in your configuration.",
			len(helmCommands), totalHAs, totalHAs)
	}
	if err := validateRancherHelmCommandsUseExternalTLS(helmCommands); err != nil {
		t.Fatalf("Rancher Helm command preflight failed before provisioning infrastructure: %v", err)
	}

	if err := validateLocalToolingPreflight(helmCommands); err != nil {
		t.Fatalf("Local tooling preflight failed before provisioning infrastructure: %v", err)
	}

	if err := validateWebhookImagePreflight(); err != nil {
		t.Fatalf("Webhook image preflight failed before provisioning infrastructure: %v", err)
	}

	if err := validateSecretEnvironment(); err != nil {
		t.Fatalf("Secret environment preflight failed before provisioning infrastructure: %v", err)
	}

	if err := validatePinnedRKE2InstallerChecksum(resolvedPlans); err != nil {
		t.Fatalf("RKE2 installer checksum preflight failed before provisioning infrastructure: %v", err)
	}

	for i, plan := range resolvedPlans {
		if err := writeRancherResolutionArtifact("install", i+1, plan); err != nil {
			t.Fatalf("Failed to write Rancher install resolution artifact: %v", err)
		}
	}

	terraformOptions := getTerraformOptions(t, totalHAs)
	terraform.InitAndApply(t, terraformOptions)

	outputs := getTerraformOutputs(t, terraformOptions)
	if len(outputs) == 0 {
		t.Fatal("No outputs received from terraform")
	}

	var wg sync.WaitGroup
	var setupErr error
	var setupErrMutex sync.Mutex

	for i := 1; i <= totalHAs; i++ {
		wg.Add(1)
		instanceNum := i

		go func(instanceNum int) {
			defer wg.Done()

			log.Printf("Starting setup for HA instance %d", instanceNum)

			t.Run(fmt.Sprintf("HA%d", instanceNum), func(subT *testing.T) {
				var resolvedPlan *RancherResolvedPlan
				if len(resolvedPlans) >= instanceNum {
					resolvedPlan = resolvedPlans[instanceNum-1]
				}
				if err := setupHAInstance(subT, instanceNum, outputs, resolvedPlan); err != nil {
					setupErrMutex.Lock()
					setupErr = fmt.Errorf("HA instance %d setup failed: %s", instanceNum, err.Error())
					setupErrMutex.Unlock()
					subT.Fail()
				}
			})
		}(instanceNum)
	}

	wg.Wait()

	if setupErr != nil {
		t.Fatalf("Error during parallel HA setup: %v", setupErr)
	}

	logHASummary(totalHAs, outputs, resolvedPlans)
}

func TestHACleanup(t *testing.T) {
	requireExplicitLifecycleTest(t, "TestHACleanup")
	setupConfig(t)
	totalHAs := viper.GetInt("total_has")
	if err := validateSecretEnvironment(); err != nil {
		t.Fatalf("Secret environment preflight failed before cleanup: %v", err)
	}

	terraformOptions := getTerraformOptions(t, totalHAs)
	var costEstimate *cleanupCostEstimate
	outputs, outputsErr := getTerraformOutputsE(t, terraformOptions)
	if outputsErr != nil {
		log.Printf("[cleanup] Terraform outputs unavailable before destroy, likely no infrastructure was applied yet: %v", outputsErr)
	} else {
		var estimateErr error
		costEstimate, estimateErr = estimateCurrentRunCost(totalHAs, outputs)
		if estimateErr != nil {
			log.Printf("[cleanup] Could not estimate EC2/EBS cost before destroy: %v", estimateErr)
		}
	}
	terraform.Destroy(t, terraformOptions)

	for i := 1; i <= totalHAs; i++ {
		cleanupHAInstance(i)
	}
	cleanupTerraformFiles()
	cleanupAutomationOutput()

	if costEstimate != nil {
		logCleanupCostEstimate(costEstimate)
	}
}

func TestHAControlPanel(t *testing.T) {
	requireExplicitLifecycleTest(t, "TestHAControlPanel")
	runHAControlPanelTest(t)
}
