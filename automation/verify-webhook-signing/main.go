package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	slsaProvenanceType = "https://slsa.dev/provenance/v1"
	cosignSignType     = "https://sigstore.dev/cosign/sign/v1"
)

type plan struct {
	TargetVersion string `json:"target_version"`
	WebhookImage  string `json:"webhook_image"`
	SigningPolicy string `json:"signing_policy"`
}

type planSet struct {
	Plans []plan `json:"plans"`
}

type verificationClaim struct {
	Critical struct {
		Type string `json:"type"`
	} `json:"critical"`
}

type signingResult struct {
	TargetVersion      string   `json:"target_version"`
	WebhookImage       string   `json:"webhook_image"`
	SigningPolicy      string   `json:"signing_policy"`
	Tool               string   `json:"tool"`
	Enforced           bool     `json:"enforced"`
	SignatureVerified  bool     `json:"signature_verified"`
	ProvenanceVerified bool     `json:"provenance_verified"`
	SBOMVerified       bool     `json:"sbom_verified"`
	ClaimTypes         []string `json:"claim_types,omitempty"`
	VerifiedAt         string   `json:"verified_at"`
}

func main() {
	var planPath string
	var slsactlPath string
	var outputPath string
	var timeout time.Duration

	flag.StringVar(&planPath, "plan", "signoff-plan.json", "sign-off plan JSON path")
	flag.StringVar(&slsactlPath, "slsactl", "slsactl", "slsactl executable path")
	flag.StringVar(&outputPath, "output", "", "optional JSON result output path")
	flag.DurationVar(&timeout, "timeout", 5*time.Minute, "timeout for each slsactl command")
	flag.Parse()

	plans, err := readPlans(planPath)
	if err != nil {
		fatalf("read plan: %v", err)
	}
	if len(plans) == 0 {
		fatalf("plan %s did not contain any plans", planPath)
	}

	var results []signingResult
	for _, plan := range plans {
		result, err := verifyPlan(plan, slsactlPath, timeout)
		if err != nil {
			fatalf("%v", err)
		}
		results = append(results, result)
	}
	if outputPath != "" {
		if err := writeResults(outputPath, results); err != nil {
			fatalf("write signing results: %v", err)
		}
	}
}

func readPlans(path string) ([]plan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var single plan
	if err := json.Unmarshal(data, &single); err == nil && single.TargetVersion != "" {
		return []plan{single}, nil
	}

	var set planSet
	if err := json.Unmarshal(data, &set); err != nil {
		return nil, err
	}
	return set.Plans, nil
}

func verifyPlan(plan plan, slsactlPath string, timeout time.Duration) (signingResult, error) {
	policy := strings.ToLower(strings.TrimSpace(plan.SigningPolicy))
	if policy == "" {
		policy = "report-only"
	}
	result := signingResult{
		TargetVersion: plan.TargetVersion,
		WebhookImage:  plan.WebhookImage,
		SigningPolicy: policy,
		Tool:          "slsactl",
		VerifiedAt:    time.Now().UTC().Format(time.RFC3339),
	}
	if policy == "skip" || policy == "report-only" {
		fmt.Printf("[signing] %s webhook image signing policy is %s; not enforcing Sigstore/SBOM verification\n", plan.TargetVersion, policy)
		return result, nil
	}
	if policy != "required" {
		return result, fmt.Errorf("%s has unsupported signing policy %q", plan.TargetVersion, plan.SigningPolicy)
	}
	if strings.TrimSpace(plan.WebhookImage) == "" {
		return result, fmt.Errorf("%s has signing_policy=required but no webhook_image", plan.TargetVersion)
	}
	result.Enforced = true

	fmt.Printf("[signing] Verifying %s webhook image %s with slsactl\n", plan.TargetVersion, plan.WebhookImage)
	verifyOutput, err := runCommand(timeout, slsactlPath, "verify", plan.WebhookImage)
	if err != nil {
		return result, fmt.Errorf("webhook image signature verification failed for %s: %w\n%s", plan.WebhookImage, err, limitOutput(verifyOutput))
	}
	claimTypes, err := validateVerifyOutput(verifyOutput)
	if err != nil {
		return result, fmt.Errorf("webhook image signature verification output did not include expected claims for %s: %w\n%s", plan.WebhookImage, err, limitOutput(verifyOutput))
	}
	result.ClaimTypes = claimTypes
	result.SignatureVerified = containsString(claimTypes, cosignSignType)
	result.ProvenanceVerified = containsString(claimTypes, slsaProvenanceType)
	fmt.Printf("[signing] Verified signature and SLSA provenance for %s\n", plan.WebhookImage)

	sbomOutput, err := runCommand(timeout, slsactlPath, "download", "sbom", plan.WebhookImage)
	if err != nil {
		return result, fmt.Errorf("webhook image SBOM download failed for %s: %w\n%s", plan.WebhookImage, err, limitOutput(sbomOutput))
	}
	if err := validateSBOMOutput(sbomOutput); err != nil {
		return result, fmt.Errorf("webhook image SBOM output was not recognized for %s: %w\n%s", plan.WebhookImage, err, limitOutput(sbomOutput))
	}
	result.SBOMVerified = true
	fmt.Printf("[signing] Verified SBOM is available for %s\n", plan.WebhookImage)
	return result, nil
}

func runCommand(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if ctx.Err() != nil {
		return out.String(), ctx.Err()
	}
	return out.String(), err
}

func validateVerifyOutput(output string) ([]string, error) {
	var claims []verificationClaim
	if err := unmarshalJSONArray(output, &claims); err != nil {
		if strings.Contains(output, slsaProvenanceType) && strings.Contains(output, cosignSignType) {
			return []string{cosignSignType, slsaProvenanceType}, nil
		}
		return nil, err
	}

	seen := map[string]bool{}
	for _, claim := range claims {
		seen[claim.Critical.Type] = true
	}
	var missing []string
	for _, want := range []string{slsaProvenanceType, cosignSignType} {
		if !seen[want] {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing %s", strings.Join(missing, ", "))
	}
	claimTypes := make([]string, 0, len(seen))
	for claimType := range seen {
		claimTypes = append(claimTypes, claimType)
	}
	sort.Strings(claimTypes)
	return claimTypes, nil
}

func validateSBOMOutput(output string) error {
	var sbom map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &sbom); err != nil {
		return err
	}
	if len(sbom) == 0 {
		return errors.New("empty SBOM document")
	}
	if _, ok := sbom["SPDXID"]; ok {
		return nil
	}
	if _, ok := sbom["spdxVersion"]; ok {
		return nil
	}
	if _, ok := sbom["creationInfo"]; ok {
		return nil
	}
	return errors.New("SBOM document did not include SPDXID, spdxVersion, or creationInfo")
}

func unmarshalJSONArray(output string, v interface{}) error {
	start := strings.IndexByte(output, '[')
	end := strings.LastIndexByte(output, ']')
	if start < 0 || end < start {
		return errors.New("no JSON array found")
	}
	return json.Unmarshal([]byte(output[start:end+1]), v)
}

func limitOutput(output string) string {
	output = strings.TrimSpace(output)
	if len(output) <= 4096 {
		return output
	}
	return output[:4096] + "\n[output truncated]"
}

func writeResults(path string, results []signingResult) error {
	var value interface{}
	if len(results) == 1 {
		value = results[0]
	} else {
		value = struct {
			Results []signingResult `json:"results"`
		}{Results: results}
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, data, 0o644)
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func fatalf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
