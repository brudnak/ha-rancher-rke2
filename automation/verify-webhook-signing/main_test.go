package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVerifyPlanRequiresSignatureAndSBOM(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	slsactl := writeFakeSLSACTL(t, dir, logPath, `
case "$1" in
  verify)
    echo 'Verification for image --'
    echo '[{"critical":{"type":"https://slsa.dev/provenance/v1"}},{"critical":{"type":"https://sigstore.dev/cosign/sign/v1"}}]'
    ;;
  download)
    echo '{"SPDXID":"SPDXRef-DOCUMENT","creationInfo":{"created":"2026-04-26T00:00:00Z"}}'
    ;;
esac
`)

	result, err := verifyPlan(plan{
		TargetVersion: "v2.14.1-alpha7",
		WebhookImage:  "stgregistry.suse.com/rancher/rancher-webhook:v0.10.1-rc.5",
		SigningPolicy: "required",
	}, slsactl, 10*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Enforced || !result.SignatureVerified || !result.ProvenanceVerified || !result.SBOMVerified {
		t.Fatalf("expected enforced signing result with all checks true, got %+v", result)
	}
	if !containsString(result.ClaimTypes, slsaProvenanceType) || !containsString(result.ClaimTypes, cosignSignType) {
		t.Fatalf("expected both claim types, got %+v", result.ClaimTypes)
	}

	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read calls: %v", err)
	}
	got := string(calls)
	if !strings.Contains(got, "verify stgregistry.suse.com/rancher/rancher-webhook:v0.10.1-rc.5") {
		t.Fatalf("expected verify call, got %s", got)
	}
	if !strings.Contains(got, "download sbom stgregistry.suse.com/rancher/rancher-webhook:v0.10.1-rc.5") {
		t.Fatalf("expected SBOM call, got %s", got)
	}
}

func TestVerifyPlanFailsWhenSBOMIsMissing(t *testing.T) {
	dir := t.TempDir()
	slsactl := writeFakeSLSACTL(t, dir, filepath.Join(dir, "calls.log"), `
case "$1" in
  verify)
    echo '[{"critical":{"type":"https://slsa.dev/provenance/v1"}},{"critical":{"type":"https://sigstore.dev/cosign/sign/v1"}}]'
    ;;
  download)
    echo '{}'
    ;;
esac
`)

	_, err := verifyPlan(plan{
		TargetVersion: "v2.14.1-alpha7",
		WebhookImage:  "stgregistry.suse.com/rancher/rancher-webhook:v0.10.1-rc.5",
		SigningPolicy: "required",
	}, slsactl, 10*time.Second)
	if err == nil {
		t.Fatal("expected missing SBOM to fail")
	}
	if !strings.Contains(err.Error(), "SBOM") {
		t.Fatalf("expected SBOM error, got %v", err)
	}
}

func TestVerifyPlanSkipsReportOnly(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	slsactl := writeFakeSLSACTL(t, dir, logPath, `exit 1`)

	result, err := verifyPlan(plan{
		TargetVersion: "v2.14.1-alpha7",
		WebhookImage:  "docker.io/rancher/rancher-webhook:v0.10.1-rc.5",
		SigningPolicy: "report-only",
	}, slsactl, 10*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Enforced || result.SignatureVerified || result.SBOMVerified {
		t.Fatalf("expected report-only signing result to be non-enforced, got %+v", result)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("expected fake slsactl not to be called, stat err=%v", err)
	}
}

func TestWriteResultsWritesSingleResult(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "automation-output", "webhook-signing.json")
	err := writeResults(path, []signingResult{{
		TargetVersion:      "v2.14.1-alpha7",
		WebhookImage:       "stgregistry.suse.com/rancher/rancher-webhook:v0.10.1-rc.5",
		SigningPolicy:      "required",
		Tool:               "slsactl",
		Enforced:           true,
		SignatureVerified:  true,
		ProvenanceVerified: true,
		SBOMVerified:       true,
		ClaimTypes:         []string{slsaProvenanceType, cosignSignType},
		VerifiedAt:         "2026-04-26T00:00:00Z",
	}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	got := string(data)
	for _, want := range []string{
		`"tool": "slsactl"`,
		`"signature_verified": true`,
		`"provenance_verified": true`,
		`"sbom_verified": true`,
		slsaProvenanceType,
		cosignSignType,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected result to contain %s:\n%s", want, got)
		}
	}
}

func TestReadPlansSupportsPlanSet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plans.json")
	if err := os.WriteFile(path, []byte(`{"plans":[{"target_version":"v2.14.1-alpha7","webhook_image":"image","signing_policy":"required"}]}`), 0o644); err != nil {
		t.Fatalf("write plan: %v", err)
	}

	plans, err := readPlans(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plans) != 1 || plans[0].TargetVersion != "v2.14.1-alpha7" {
		t.Fatalf("unexpected plans: %+v", plans)
	}
}

func writeFakeSLSACTL(t *testing.T, dir, logPath, body string) string {
	t.Helper()
	path := filepath.Join(dir, "slsactl")
	script := "#!/bin/sh\n" +
		"set -eu\n" +
		"echo \"$@\" >> " + shellQuote(logPath) + "\n" +
		body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake slsactl: %v", err)
	}
	return path
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
