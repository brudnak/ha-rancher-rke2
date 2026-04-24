package test

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/brudnak/ha-rancher-rke2/terratest/hcl"
	"github.com/gruntwork-io/terratest/modules/terraform"
	"github.com/spf13/viper"
)

func setupConfig(t *testing.T) {
	viper.Reset()
	viper.AddConfigPath("../")
	viper.SetConfigName("tool-config")
	viper.SetConfigType("yml")

	if err := viper.ReadInConfig(); err != nil {
		t.Fatalf("Failed to read config: %v", err)
	}
}

func getTerraformOptions(t *testing.T, totalHAs int) *terraform.Options {
	generateAwsVars()

	backendConfig, err := terraformBackendConfigFromEnv()
	if err != nil {
		t.Fatalf("Invalid Terraform backend environment: %v", err)
	}

	return terraform.WithDefaultRetryableErrors(t, &terraform.Options{
		TerraformDir:  "../modules/aws",
		NoColor:       true,
		BackendConfig: backendConfig,
		Vars: map[string]interface{}{
			"total_has":             totalHAs,
			"aws_prefix":            viper.GetString("tf_vars.aws_prefix"),
			"aws_vpc":               viper.GetString("tf_vars.aws_vpc"),
			"aws_subnet_a":          viper.GetString("tf_vars.aws_subnet_a"),
			"aws_subnet_b":          viper.GetString("tf_vars.aws_subnet_b"),
			"aws_subnet_c":          viper.GetString("tf_vars.aws_subnet_c"),
			"aws_ami":               viper.GetString("tf_vars.aws_ami"),
			"aws_subnet_id":         viper.GetString("tf_vars.aws_subnet_id"),
			"aws_security_group_id": viper.GetString("tf_vars.aws_security_group_id"),
			"aws_pem_key_name":      viper.GetString("tf_vars.aws_pem_key_name"),
			"aws_route53_fqdn":      viper.GetString("tf_vars.aws_route53_fqdn"),
		},
	})
}

func terraformBackendConfigFromEnv() (map[string]interface{}, error) {
	values := map[string]string{
		"TF_STATE_BUCKET":     strings.TrimSpace(os.Getenv("TF_STATE_BUCKET")),
		"TF_STATE_LOCK_TABLE": strings.TrimSpace(os.Getenv("TF_STATE_LOCK_TABLE")),
		"TF_STATE_REGION":     strings.TrimSpace(os.Getenv("TF_STATE_REGION")),
		"TF_STATE_KEY":        strings.TrimSpace(os.Getenv("TF_STATE_KEY")),
	}

	anySet := false
	var missing []string
	for key, value := range values {
		if value != "" {
			anySet = true
			continue
		}
		missing = append(missing, key)
	}

	if !anySet {
		return nil, nil
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("set all remote backend env vars or none; missing %s", strings.Join(missing, ", "))
	}

	return map[string]interface{}{
		"bucket":         values["TF_STATE_BUCKET"],
		"key":            values["TF_STATE_KEY"],
		"region":         values["TF_STATE_REGION"],
		"dynamodb_table": values["TF_STATE_LOCK_TABLE"],
		"encrypt":        true,
	}, nil
}

func generateAwsVars() {
	hcl.GenAwsVar(
		viper.GetString("tf_vars.aws_prefix"),
		viper.GetString("tf_vars.aws_vpc"),
		viper.GetString("tf_vars.aws_subnet_a"),
		viper.GetString("tf_vars.aws_subnet_b"),
		viper.GetString("tf_vars.aws_subnet_c"),
		viper.GetString("tf_vars.aws_ami"),
		viper.GetString("tf_vars.aws_subnet_id"),
		viper.GetString("tf_vars.aws_security_group_id"),
		viper.GetString("tf_vars.aws_pem_key_name"),
		viper.GetString("tf_vars.aws_route53_fqdn"),
	)
}

func getTerraformOutputs(t *testing.T, terraformOptions *terraform.Options) map[string]string {
	output := terraform.OutputJson(t, terraformOptions, "flat_outputs")

	var outputs map[string]string
	if err := json.Unmarshal([]byte(output), &outputs); err != nil {
		if t != nil {
			t.Logf("Raw output: %s", output)
			t.Fatalf("Failed to parse terraform outputs: %v", err)
		}
		log.Printf("Raw output: %s", output)
		return nil
	}

	return outputs
}

func getHAOutputs(instanceNum int, outputs map[string]string) TerraformOutputs {
	prefix := fmt.Sprintf("ha_%d", instanceNum)
	return TerraformOutputs{
		Server1IP:        outputs[fmt.Sprintf("%s_server1_ip", prefix)],
		Server2IP:        outputs[fmt.Sprintf("%s_server2_ip", prefix)],
		Server3IP:        outputs[fmt.Sprintf("%s_server3_ip", prefix)],
		Server1PrivateIP: outputs[fmt.Sprintf("%s_server1_private_ip", prefix)],
		Server2PrivateIP: outputs[fmt.Sprintf("%s_server2_private_ip", prefix)],
		Server3PrivateIP: outputs[fmt.Sprintf("%s_server3_private_ip", prefix)],
		LoadBalancerDNS:  outputs[fmt.Sprintf("%s_aws_lb", prefix)],
		RancherURL:       outputs[fmt.Sprintf("%s_rancher_url", prefix)],
	}
}

func logHASummary(totalHAs int, outputs map[string]string, resolvedPlans []*RancherResolvedPlan) {
	log.Printf("HA setup complete. Rancher URLs:")
	for i := 1; i <= totalHAs; i++ {
		haOutputs := getHAOutputs(i, outputs)
		requestedVersion := ""
		if len(resolvedPlans) >= i && resolvedPlans[i-1] != nil {
			requestedVersion = resolvedPlans[i-1].RequestedVersion
		}
		if requestedVersion != "" {
			log.Printf("Rancher instance %d (%s) -> %s", i, requestedVersion, clickableURL(haOutputs.RancherURL))
			continue
		}
		log.Printf("Rancher instance %d -> %s", i, clickableURL(haOutputs.RancherURL))
	}
}
