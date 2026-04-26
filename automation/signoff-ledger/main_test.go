package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLedgerRecordsSuccessfulLane(t *testing.T) {
	tempDir := t.TempDir()
	planPath := filepath.Join(tempDir, "signoff-plan.json")
	ledgerPath := filepath.Join(tempDir, "signoff-ledger.json")
	signingPath := filepath.Join(tempDir, "webhook-signing.json")
	planJSON := `{
  "target_version": "v2.14.1-alpha7",
  "release_line": "v2.14",
  "previous_version": "v2.14.0",
  "target_webhook_build": "109.0.1+up0.10.1-rc.5",
  "target_webhook_tag": "v0.10.1-rc.5",
  "previous_webhook_build": "109.0.0+up0.10.0",
  "previous_webhook_tag": "v0.10.0",
  "webhook_changed": true,
  "webhook_image": "stgregistry.suse.com/rancher/rancher-webhook:v0.10.1-rc.5",
  "signing_policy": "required",
  "signing_registry": "stgregistry.suse.com",
  "lanes": [
    {
      "name": "fresh-alpha",
      "install_rancher": "v2.14.1-alpha7"
    }
  ]
}`
	if err := os.WriteFile(planPath, []byte(planJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	signingJSON := `{
  "target_version": "v2.14.1-alpha7",
  "webhook_image": "stgregistry.suse.com/rancher/rancher-webhook:v0.10.1-rc.5",
  "signing_policy": "required",
  "tool": "slsactl",
  "enforced": true,
  "signature_verified": true,
  "provenance_verified": true,
  "sbom_verified": true,
  "claim_types": [
    "https://sigstore.dev/cosign/sign/v1",
    "https://slsa.dev/provenance/v1"
  ],
  "verified_at": "2026-04-26T00:00:00Z"
}`
	if err := os.WriteFile(signingPath, []byte(signingJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	plan, err := readPlan(planPath)
	if err != nil {
		t.Fatal(err)
	}
	lane, err := findLane(plan, "fresh-alpha")
	if err != nil {
		t.Fatal(err)
	}
	l, err := readLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	signingResult, err := readSigningResult(signingPath)
	if err != nil {
		t.Fatal(err)
	}
	l.Entries[plan.TargetVersion] = map[string]entry{
		lane.Name: {
			Status:               "success",
			CoveragePolicy:       currentCoveragePolicy,
			RunID:                "123",
			Lane:                 lane.Name,
			ReleaseLine:          plan.ReleaseLine,
			TargetVersion:        plan.TargetVersion,
			InstallRancher:       lane.InstallRancher,
			WebhookImage:         plan.WebhookImage,
			PreviousWebhookBuild: plan.PreviousWebhookBuild,
			PreviousWebhookTag:   plan.PreviousWebhookTag,
			TargetWebhookBuild:   plan.TargetWebhookBuild,
			TargetWebhookTag:     plan.TargetWebhookTag,
			SigningPolicy:        plan.SigningPolicy,
			SigningRegistry:      plan.SigningRegistry,
			SigningVerification:  signingResult,
			CompletedAt:          "2026-04-25T00:00:00Z",
		},
	}
	if err := writeLedger(ledgerPath, l); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{
		`"schema_version": 2`,
		`"coverage_policy": "alpha-webhook-signoff-v2"`,
		`"v2.14.1-alpha7"`,
		`"fresh-alpha"`,
		`"status": "success"`,
		`"webhook_image": "stgregistry.suse.com/rancher/rancher-webhook:v0.10.1-rc.5"`,
		`"target_webhook_build": "109.0.1+up0.10.1-rc.5"`,
		`"previous_webhook_build": "109.0.0+up0.10.0"`,
		`"signing_policy": "required"`,
		`"signing_registry": "stgregistry.suse.com"`,
		`"signing_verification": {`,
		`"tool": "slsactl"`,
		`"signature_verified": true`,
		`"provenance_verified": true`,
		`"sbom_verified": true`,
		`"https://sigstore.dev/cosign/sign/v1"`,
		`"https://slsa.dev/provenance/v1"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected ledger to contain %s:\n%s", want, got)
		}
	}
}

func TestReadSigningResultMissingPathIsOptional(t *testing.T) {
	result, err := readSigningResult(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Fatalf("expected nil result, got %+v", result)
	}
}
