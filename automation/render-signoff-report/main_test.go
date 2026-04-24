package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRenderReportIncludesNonSecretMetadata(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "downstream-ha-1.json"), `{
  "ha_index": 1,
  "cluster_name": "ha-test",
  "management_cluster_id": "c-m-abc",
  "k3s_version": "v1.33.4+k3s1",
  "linode_region": "us-ord",
  "linode_type": "g6-standard-2"
}`)
	mustWrite(t, filepath.Join(dir, "webhook-override-downstream-ha-1.json"), `{
  "scope": "downstream",
  "ha_index": 1,
  "cluster_name": "ha-test",
  "namespace": "cattle-system",
  "deployment": "rancher-webhook",
  "container": "rancher-webhook",
  "previous_image": "old",
  "candidate_image": "new"
}`)
	mustWrite(t, filepath.Join(dir, "rancher-test-results.json"), `{
  "repo": "https://github.com/rancher/tests.git",
  "ref": "main",
  "lane": "fresh-alpha",
  "rancher_version": "v2.14.1-alpha6",
  "results": [
    {
      "suite": "charts-webhook",
      "package": "./validation/charts",
      "test_run": "TestWebhookTestSuite",
      "junit": "test-results/charts-webhook.xml",
      "conclusion": "success"
    }
  ]
}`)

	report, err := renderReport(signoffPlan{
		TargetVersion:      "v2.14.1-alpha6",
		PreviousVersion:    "v2.14.0",
		TargetWebhookTag:   "v0.10.1-rc.5",
		PreviousWebhookTag: "v0.10.0",
		WebhookChanged:     true,
		WebhookImage:       "stgregistry.suse.com/rancher/rancher-webhook:v0.10.1-rc.5",
		SigningPolicy:      "required",
		SigningRegistry:    "stgregistry.suse.com",
		Lanes: []signoffLane{{
			Name:                "fresh-alpha",
			InstallRancher:      "v2.14.1-alpha6",
			ProvisionDownstream: true,
		}},
	}, dir, "fresh-alpha", time.Date(2026, 4, 24, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# v2.14.1-alpha6 Sign-Off Report", "`ha-test`", "`c-m-abc`", "`stgregistry.suse.com/rancher/rancher-webhook:v0.10.1-rc.5`", "## Rancher Test Results", "`charts-webhook`", "`success`"} {
		if !strings.Contains(report, want) {
			t.Fatalf("expected report to contain %q:\n%s", want, report)
		}
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
