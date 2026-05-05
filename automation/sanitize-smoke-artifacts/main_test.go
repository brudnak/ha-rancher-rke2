package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareArtifactsSanitizesPublicBundle(t *testing.T) {
	sourceDir := t.TempDir()
	outputDir := filepath.Join(t.TempDir(), "public")

	writeTestFile(t, filepath.Join(sourceDir, "signoff-plan.json"), `{
  "target_version": "v2.14.1-alpha12",
  "run_id": "25132790833",
  "state_key_root": "ha-rancher-rke2/signoff",
  "lanes": [
    {
      "name": "upgrade-alpha",
      "terraform_state_key": "ha-rancher-rke2/signoff/v2.14/terraform.tfstate",
      "aws_prefix": "gha-32790833-ua"
    }
  ]
}`)
	writeTestFile(t, filepath.Join(sourceDir, "lane.env"), "TF_STATE_KEY=ha-rancher-rke2/signoff/state.tfstate\n")
	writeTestFile(t, filepath.Join(sourceDir, "automation-output", "downstream-ha-1.json"), `{
  "ha_index": 1,
  "cluster_name": "gha-32790833-ha1-61abcbed",
  "kubeconfig_path": "/home/runner/work/repo/automation-output/downstream-ha-1.kubeconfig",
  "rancher_host": "https://gha-32790833-ua-1-hippo-4e22.qa.rancher.space",
  "secret_name": "cc-gha-32790833-ha1-61abcbed",
  "linode_region": "us-ord",
  "k3s_version": "v1.35.3+k3s1"
}`)
	writeTestFile(t, filepath.Join(sourceDir, "automation-output", "webhook-signing.json"), `{
  "target_version": "v2.14.1-alpha12",
  "claim_types": ["https://sigstore.dev/cosign/sign/v1"],
  "signature_verified": true
}`)
	writeTestFile(t, filepath.Join(sourceDir, "automation-output", "local-suite-ha-1.json"), `{
  "ha_index": 1,
  "rancher_host": "<masked>",
  "cluster_name": "local"
}`)
	writeTestFile(t, filepath.Join(sourceDir, "automation-output", "webhook-override-downstream-ha-1.json"), `{
  "scope": "downstream",
  "ha_index": 1,
  "cluster_name": "gha-32790833-ha1-61abcbed",
  "namespace": "cattle-system",
  "deployment": "rancher-webhook",
  "container": "rancher-webhook",
  "previous_image": "registry.example.invalid/rancher-webhook:old",
  "candidate_image": "stgregistry.suse.com/rancher/rancher-webhook:v0.10.4-rc.1",
  "rollout_complete": true
}`)
	writeTestFile(t, filepath.Join(sourceDir, "automation-output", "signoff-dispatch-results.json"), `[
  {
    "rancher_version": "v2.14.1-alpha12",
    "lane": "upgrade-alpha",
    "observed_run": {
      "databaseId": 25132790833,
      "url": "https://github.com/brudnak/ha-rancher-rke2/actions/runs/25132790833",
      "displayTitle": "Run v2.14.1-alpha12 / upgrade-alpha"
    }
  }
]`)
	writeTestFile(t, filepath.Join(sourceDir, "automation-output", "signoff-report.md"), "Host https://gha-32790833-ua-1-hippo-4e22.qa.rancher.space cluster gha-32790833-ha1-61abcbed\n")
	writeTestFile(t, filepath.Join(sourceDir, "test-results", "suite.xml"), `<testcase name="Verify_on_gha-32790833-ha1-61abcbed"></testcase>`)

	if err := prepareArtifacts(sourceDir, outputDir); err != nil {
		t.Fatalf("prepare artifacts: %v", err)
	}

	assertNoFile(t, filepath.Join(outputDir, "lane.env"))
	assertFileOmits(t, filepath.Join(outputDir, "signoff-plan.json"), "terraform_state_key", "state_key_root", "aws_prefix", "run_id")
	assertFileOmits(t, filepath.Join(outputDir, "automation-output", "downstream-ha-1.json"), "rancher_host", "kubeconfig_path", "secret_name", "linode_region", "gha-32790833")
	assertFileOmits(t, filepath.Join(outputDir, "automation-output", "webhook-signing.json"), "claim_types", "https://sigstore.dev")
	assertFileOmits(t, filepath.Join(outputDir, "automation-output", "local-suite-ha-1.json"), "rancher_host", "cluster_name", "local")
	assertFileOmits(t, filepath.Join(outputDir, "automation-output", "webhook-override-downstream-ha-1.json"), "cluster_name", "namespace", "deployment", "container", "previous_image", "candidate_image", "gha-32790833", "registry.example.invalid")
	assertFileOmits(t, filepath.Join(outputDir, "automation-output", "signoff-dispatch-results.json"), "databaseId", "url", "https://github.com", "25132790833")
	assertFileOmits(t, filepath.Join(outputDir, "automation-output", "signoff-report.md"), "https://", "gha-32790833", "qa.rancher.space")
	assertFileOmits(t, filepath.Join(outputDir, "test-results", "suite.xml"), "gha-32790833")
	assertFileOmits(t, filepath.Join(outputDir, "artifact-summary.md"), outputDir)

	var plan map[string]interface{}
	readJSON(t, filepath.Join(outputDir, "signoff-plan.json"), &plan)
	if plan["target_version"] != "v2.14.1-alpha12" {
		t.Fatalf("expected target_version to be preserved, got %#v", plan["target_version"])
	}
}

func TestSanitizeTextRedactsURLsAndGeneratedNames(t *testing.T) {
	input := "`https://gha-32790833-ua-1-hippo-4e22.qa.rancher.space/v3` on gha-32790833-ha1-61abcbed in c-m-lrnz9k4l"
	got := sanitizeText(input)
	for _, forbidden := range []string{"https://", "qa.rancher.space", "gha-32790833", "c-m-lrnz9k4l"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("expected %q to omit %q", got, forbidden)
		}
	}
	if !strings.Contains(got, "`<redacted-url>`") {
		t.Fatalf("expected markdown backticks to survive URL redaction, got %q", got)
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func assertNoFile(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected %s to not exist, stat err=%v", path, err)
	}
}

func assertFileOmits(t *testing.T, path string, forbidden ...string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	text := string(data)
	for _, value := range forbidden {
		if strings.Contains(text, value) {
			t.Fatalf("expected %s to omit %q:\n%s", path, value, text)
		}
	}
}

func readJSON(t *testing.T, path string, value interface{}) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(data, value); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
}
