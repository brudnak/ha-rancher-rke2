package test

import (
	"os"
	"path/filepath"
	"testing"

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

	if err := updateAutoModeConfigFile(configPath, []string{"2.14.1-alpha3", "2.13.5-alpha3", "2.12.9-alpha3"}); err != nil {
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

func TestNormalizePreflightVersionsRejectsBlankValues(t *testing.T) {
	_, err := normalizePreflightVersions([]string{"2.14.1-alpha3", "  "})
	if err == nil {
		t.Fatal("expected blank preflight version to fail validation")
	}
}
