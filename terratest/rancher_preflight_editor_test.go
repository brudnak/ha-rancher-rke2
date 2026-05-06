package test

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

func TestUpdateAutoModeConfigFileRewritesVersionsAndTotalHAs(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "tool-config.yml")

	initialConfig := `rancher:
  mode: auto
  version: "2.14.1-alpha3"
  bootstrap_password: "admin"
total_has: 1
tf_vars:
  aws_region: "us-east-2"
`
	if err := os.WriteFile(configPath, []byte(initialConfig), 0o644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	if err := updateAutoModeConfigFile(configPath, preflightConfigUpdate{
		Versions: []string{"2.14.1-alpha3", "2.13.5-alpha3", "2.12.9-alpha3"},
	}); err != nil {
		t.Fatalf("updateAutoModeConfigFile returned error: %v", err)
	}

	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("failed to read updated config: %v", err)
	}

	var parsed struct {
		Rancher  map[string]interface{} `yaml:"rancher"`
		TotalHAs int                    `yaml:"total_has"`
		TFVars   map[string]interface{} `yaml:"tf_vars"`
	}
	if err := yaml.Unmarshal(content, &parsed); err != nil {
		t.Fatalf("failed to parse updated config: %v", err)
	}

	if parsed.TotalHAs != 3 {
		t.Fatalf("expected total_has 3, got %d", parsed.TotalHAs)
	}

	rawVersions, ok := parsed.Rancher["versions"].([]interface{})
	if !ok {
		t.Fatalf("expected rancher.versions sequence, got %#v", parsed.Rancher["versions"])
	}
	if len(rawVersions) != 3 {
		t.Fatalf("expected 3 versions, got %d", len(rawVersions))
	}
	if rawVersions[0] != "2.14.1-alpha3" || rawVersions[1] != "2.13.5-alpha3" || rawVersions[2] != "2.12.9-alpha3" {
		t.Fatalf("unexpected version list: %#v", rawVersions)
	}
	if _, exists := parsed.Rancher["version"]; exists {
		t.Fatalf("expected rancher.version to be removed, but it is still present")
	}
	if parsed.TFVars["aws_region"] != "us-east-2" {
		t.Fatalf("expected unrelated tf_vars to be preserved, got %#v", parsed.TFVars)
	}
}

func TestUpdateAutoModeConfigFileWritesCustomHostnameAndNormalizesVersion(t *testing.T) {
	t.Cleanup(viper.Reset)
	viper.Reset()
	viper.Set("tf_vars.aws_route53_fqdn", "qa.rancher.space")

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "tool-config.yml")

	initialConfig := `rancher:
  mode: auto
  versions:
    - "2.14.1"
    - "2.13.5"
total_has: 2
tf_vars:
  aws_region: "us-east-2"
  aws_route53_fqdn: "qa.rancher.space"
`
	if err := os.WriteFile(configPath, []byte(initialConfig), 0o644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	if err := updateAutoModeConfigFile(configPath, preflightConfigUpdate{
		Versions:              []string{"v2.14-head"},
		CustomHostnameEnabled: true,
		CustomHostnameInput:   "https://Brudnak.qa.rancher.space/saml",
	}); err != nil {
		t.Fatalf("updateAutoModeConfigFile returned error: %v", err)
	}

	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("failed to read updated config: %v", err)
	}

	var parsed struct {
		Rancher  map[string]interface{} `yaml:"rancher"`
		TotalHAs int                    `yaml:"total_has"`
		TFVars   map[string]interface{} `yaml:"tf_vars"`
	}
	if err := yaml.Unmarshal(content, &parsed); err != nil {
		t.Fatalf("failed to parse updated config: %v", err)
	}

	if parsed.TotalHAs != 1 {
		t.Fatalf("expected total_has 1, got %d", parsed.TotalHAs)
	}
	rawVersions, ok := parsed.Rancher["versions"].([]interface{})
	if !ok || len(rawVersions) != 1 || rawVersions[0] != "2.14-head" {
		t.Fatalf("unexpected version list: %#v", parsed.Rancher["versions"])
	}
	if parsed.TFVars["custom_hostname_prefix"] != "brudnak" {
		t.Fatalf("expected custom_hostname_prefix brudnak, got %#v", parsed.TFVars["custom_hostname_prefix"])
	}
	if got := viper.GetString(customHostnameConfigKey); got != "brudnak" {
		t.Fatalf("expected viper custom hostname brudnak, got %q", got)
	}
}

func TestUpdateAutoModeConfigFileWritesEditableConfig(t *testing.T) {
	t.Cleanup(viper.Reset)
	viper.Reset()
	viper.Set("tf_vars.aws_route53_fqdn", "qa.rancher.space")

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "tool-config.yml")

	initialConfig := `rancher:
  mode: auto
  versions:
    - "2.14-head"
  distro: auto
  bootstrap_password: "old"
rke2:
  preload_images: false
total_has: 1
tf_vars:
  aws_region: "us-east-2"
  aws_prefix: "old"
  aws_vpc: ""
  aws_subnet_a: ""
  aws_subnet_b: ""
  aws_subnet_c: ""
  aws_ami: ""
  aws_subnet_id: ""
  aws_security_group_id: ""
  aws_pem_key_name: ""
  aws_route53_fqdn: ""
`
	if err := os.WriteFile(configPath, []byte(initialConfig), 0o644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	tfVars := map[string]string{
		"aws_region":            "us-west-2",
		"aws_prefix":            "ATB",
		"aws_vpc":               "vpc-123",
		"aws_subnet_a":          "subnet-a",
		"aws_subnet_b":          "subnet-b",
		"aws_subnet_c":          "subnet-c",
		"aws_ami":               "ami-123",
		"aws_subnet_id":         "subnet-node",
		"aws_security_group_id": "sg-123",
		"aws_pem_key_name":      "qa-key",
		"aws_route53_fqdn":      "qa.rancher.space",
	}
	if err := updateAutoModeConfigFile(configPath, preflightConfigUpdate{
		Versions:          []string{"head"},
		Distro:            "community",
		BootstrapPassword: "new-password",
		PreloadImages:     true,
		TFVars:            tfVars,
	}); err != nil {
		t.Fatalf("updateAutoModeConfigFile returned error: %v", err)
	}

	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("failed to read updated config: %v", err)
	}

	var parsed struct {
		Rancher map[string]interface{} `yaml:"rancher"`
		RKE2    map[string]interface{} `yaml:"rke2"`
		TFVars  map[string]interface{} `yaml:"tf_vars"`
	}
	if err := yaml.Unmarshal(content, &parsed); err != nil {
		t.Fatalf("failed to parse updated config: %v", err)
	}

	if parsed.Rancher["distro"] != "community" || parsed.Rancher["bootstrap_password"] != "new-password" {
		t.Fatalf("expected Rancher settings to be updated, got %#v", parsed.Rancher)
	}
	if parsed.RKE2["preload_images"] != true {
		t.Fatalf("expected rke2.preload_images=true, got %#v", parsed.RKE2["preload_images"])
	}
	if parsed.TFVars["aws_prefix"] != "atb" || parsed.TFVars["aws_region"] != "us-west-2" {
		t.Fatalf("expected tf_vars to be updated and prefix lowercased, got %#v", parsed.TFVars)
	}
}

func TestNormalizePreflightVersionsRejectsBlankValues(t *testing.T) {
	_, err := normalizePreflightVersions([]string{"2.14.1-alpha3", "  "})
	if err == nil {
		t.Fatal("expected blank preflight version to fail validation")
	}
}

func TestNormalizePreflightVersionsAcceptsLeadingV(t *testing.T) {
	versions, err := normalizePreflightVersions([]string{"v2.14-head", "V2.13.5"})
	if err != nil {
		t.Fatalf("normalizePreflightVersions returned error: %v", err)
	}
	if versions[0] != "2.14-head" || versions[1] != "2.13.5" {
		t.Fatalf("expected leading v to be stripped, got %#v", versions)
	}
}

func TestValidateCustomHostnameConfigRequiresSingleHA(t *testing.T) {
	t.Cleanup(viper.Reset)
	viper.Reset()
	viper.Set("tf_vars.aws_route53_fqdn", "qa.rancher.space")
	viper.Set(customHostnameConfigKey, "brudnak")

	err := validateCustomHostnameConfig(2)
	if err == nil {
		t.Fatal("expected custom hostname with multiple HAs to fail validation")
	}
}

func TestConfiguredCustomHostnameTreatsQuotedEmptyAsUnset(t *testing.T) {
	t.Cleanup(viper.Reset)
	viper.Reset()
	viper.Set(customHostnameConfigKey, ` '""' `)

	prefix, err := configuredCustomHostnamePrefix()
	if err != nil {
		t.Fatalf("configuredCustomHostnamePrefix returned error: %v", err)
	}
	if prefix != "" {
		t.Fatalf("expected quoted empty custom hostname to be unset, got %q", prefix)
	}
}

func TestNormalizeAWSPrefixRequiresTwoOrThreeLetters(t *testing.T) {
	prefix, err := normalizeAWSPrefix("ATB")
	if err != nil {
		t.Fatalf("expected ATB to be valid, got %v", err)
	}
	if prefix != "atb" {
		t.Fatalf("expected prefix to be lowercased, got %q", prefix)
	}

	for _, value := range []string{"a", "abcd", "a1", ""} {
		if _, err := normalizeAWSPrefix(value); err == nil {
			t.Fatalf("expected %q to be invalid", value)
		}
	}
}

func TestDecodePreflightConfigUpdateRequestFromHTMXForm(t *testing.T) {
	body := strings.NewReader("versions=head&versions=v2.14-head&distro=community&bootstrapPassword=secret&preloadImages=true&customHostnameEnabled=true&customHostname=demo&tfVars.aws_prefix=ATB&tfVars.aws_pem_key_name=qa-key&tfVars.aws_route53_fqdn=qa.rancher.space")
	req := httptest.NewRequest("POST", "/submit", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	update, err := decodePreflightConfigUpdateRequest(req)
	if err != nil {
		t.Fatalf("decodePreflightConfigUpdateRequest returned error: %v", err)
	}
	if len(update.Versions) != 2 || update.Versions[0] != "head" || update.Versions[1] != "v2.14-head" {
		t.Fatalf("unexpected versions: %#v", update.Versions)
	}
	if update.Distro != "community" || update.BootstrapPassword != "secret" || !update.PreloadImages || !update.CustomHostnameEnabled {
		t.Fatalf("unexpected decoded update: %#v", update)
	}
	if update.TFVars["aws_prefix"] != "ATB" || update.TFVars["aws_pem_key_name"] != "qa-key" || update.TFVars["aws_route53_fqdn"] != "qa.rancher.space" {
		t.Fatalf("unexpected tf vars: %#v", update.TFVars)
	}
}
