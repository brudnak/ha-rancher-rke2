package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	ledgerSchemaVersion   = 2
	currentCoveragePolicy = "alpha-webhook-signoff-v2"
)

type signoffPlan struct {
	TargetVersion        string        `json:"target_version"`
	ReleaseLine          string        `json:"release_line"`
	PreviousVersion      string        `json:"previous_version"`
	TargetWebhookBuild   string        `json:"target_webhook_build"`
	TargetWebhookTag     string        `json:"target_webhook_tag"`
	PreviousWebhookBuild string        `json:"previous_webhook_build"`
	PreviousWebhookTag   string        `json:"previous_webhook_tag"`
	WebhookChanged       bool          `json:"webhook_changed"`
	WebhookImage         string        `json:"webhook_image"`
	SigningPolicy        string        `json:"signing_policy"`
	SigningRegistry      string        `json:"signing_registry"`
	Lanes                []signoffLane `json:"lanes"`
}

type signoffLane struct {
	Name                 string `json:"name"`
	InstallRancher       string `json:"install_rancher"`
	UpgradeToRancher     string `json:"upgrade_to_rancher"`
	WebhookOverrideImage string `json:"webhook_override_image"`
}

type ledger struct {
	SchemaVersion int                         `json:"schema_version"`
	Entries       map[string]map[string]entry `json:"entries"`
}

type signingResult struct {
	TargetVersion      string   `json:"target_version,omitempty"`
	WebhookImage       string   `json:"webhook_image,omitempty"`
	SigningPolicy      string   `json:"signing_policy,omitempty"`
	Tool               string   `json:"tool,omitempty"`
	Enforced           bool     `json:"enforced"`
	SignatureVerified  bool     `json:"signature_verified"`
	ProvenanceVerified bool     `json:"provenance_verified"`
	SBOMVerified       bool     `json:"sbom_verified"`
	VerificationError  string   `json:"verification_error,omitempty"`
	ClaimTypes         []string `json:"claim_types,omitempty"`
	VerifiedAt         string   `json:"verified_at,omitempty"`
}

type rancherResolution struct {
	Phase                  string   `json:"phase,omitempty"`
	HAIndex                int      `json:"ha_index,omitempty"`
	RequestedVersion       string   `json:"requested_version,omitempty"`
	RequestedDistro        string   `json:"requested_distro,omitempty"`
	BuildType              string   `json:"build_type,omitempty"`
	ResolvedDistro         string   `json:"resolved_distro,omitempty"`
	ChartRepoAlias         string   `json:"chart_repo_alias,omitempty"`
	ChartVersion           string   `json:"chart_version,omitempty"`
	ChartSource            string   `json:"chart_source,omitempty"`
	RancherImage           string   `json:"rancher_image,omitempty"`
	RancherImageTag        string   `json:"rancher_image_tag,omitempty"`
	AgentImage             string   `json:"agent_image,omitempty"`
	CompatibilityBaseline  string   `json:"compatibility_baseline,omitempty"`
	RecommendedRKE2Version string   `json:"recommended_rke2_version,omitempty"`
	ResolutionNotes        []string `json:"resolution_notes,omitempty"`
}

type entry struct {
	Status               string             `json:"status"`
	CoveragePolicy       string             `json:"coverage_policy"`
	RunID                string             `json:"run_id"`
	RunURL               string             `json:"run_url,omitempty"`
	Workflow             string             `json:"workflow,omitempty"`
	Lane                 string             `json:"lane"`
	ReleaseLine          string             `json:"release_line"`
	TargetVersion        string             `json:"target_version"`
	PreviousVersion      string             `json:"previous_version,omitempty"`
	InstallRancher       string             `json:"install_rancher"`
	UpgradeToRancher     string             `json:"upgrade_to_rancher,omitempty"`
	WebhookChanged       bool               `json:"webhook_changed"`
	WebhookImage         string             `json:"webhook_image,omitempty"`
	WebhookOverride      string             `json:"webhook_override_image,omitempty"`
	PreviousWebhookBuild string             `json:"previous_webhook_build,omitempty"`
	PreviousWebhookTag   string             `json:"previous_webhook_tag,omitempty"`
	TargetWebhookBuild   string             `json:"target_webhook_build,omitempty"`
	TargetWebhookTag     string             `json:"target_webhook_tag,omitempty"`
	SigningPolicy        string             `json:"signing_policy,omitempty"`
	SigningRegistry      string             `json:"signing_registry,omitempty"`
	SigningVerification  *signingResult     `json:"signing_verification,omitempty"`
	InstallResolution    *rancherResolution `json:"rancher_install_resolution,omitempty"`
	UpgradeResolution    *rancherResolution `json:"rancher_upgrade_resolution,omitempty"`
	CommitSHA            string             `json:"commit_sha,omitempty"`
	CompletedAt          string             `json:"completed_at"`
}

func main() {
	var planPath string
	var ledgerPath string
	var laneName string
	var status string
	var runID string
	var runURL string
	var workflow string
	var commitSHA string
	var completedAt string
	var signingResultPath string
	var installResolutionPath string
	var upgradeResolutionPath string

	flag.StringVar(&planPath, "plan", "signoff-plan.json", "sign-off plan JSON path")
	flag.StringVar(&ledgerPath, "ledger", "signoff-ledger.json", "sign-off ledger JSON path")
	flag.StringVar(&laneName, "lane", "", "lane name to record")
	flag.StringVar(&status, "status", "success", "lane status")
	flag.StringVar(&runID, "run-id", os.Getenv("GITHUB_RUN_ID"), "GitHub Actions run id")
	flag.StringVar(&runURL, "run-url", "", "GitHub Actions run URL")
	flag.StringVar(&workflow, "workflow", os.Getenv("GITHUB_WORKFLOW"), "GitHub Actions workflow name")
	flag.StringVar(&commitSHA, "commit-sha", os.Getenv("GITHUB_SHA"), "commit SHA tested by the run")
	flag.StringVar(&completedAt, "completed-at", "", "completion time in RFC3339; defaults to now")
	flag.StringVar(&signingResultPath, "signing-result", "", "optional webhook signing verification result JSON path")
	flag.StringVar(&installResolutionPath, "install-resolution", "", "optional Rancher install resolution JSON path")
	flag.StringVar(&upgradeResolutionPath, "upgrade-resolution", "", "optional Rancher upgrade resolution JSON path")
	flag.Parse()

	if strings.TrimSpace(laneName) == "" {
		fatalf("-lane is required")
	}
	if strings.TrimSpace(completedAt) == "" {
		completedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if _, err := time.Parse(time.RFC3339, completedAt); err != nil {
		fatalf("invalid -completed-at: %v", err)
	}

	plan, err := readPlan(planPath)
	if err != nil {
		fatalf("read plan: %v", err)
	}
	lane, err := findLane(plan, laneName)
	if err != nil {
		fatalf("find lane: %v", err)
	}
	l, err := readLedger(ledgerPath)
	if err != nil {
		fatalf("read ledger: %v", err)
	}
	signingResult, err := readSigningResult(signingResultPath)
	if err != nil {
		fatalf("read signing result: %v", err)
	}
	installResolution, err := readRancherResolution(installResolutionPath)
	if err != nil {
		fatalf("read install resolution: %v", err)
	}
	upgradeResolution, err := readRancherResolution(upgradeResolutionPath)
	if err != nil {
		fatalf("read upgrade resolution: %v", err)
	}
	l.SchemaVersion = ledgerSchemaVersion
	if l.Entries == nil {
		l.Entries = map[string]map[string]entry{}
	}
	if l.Entries[plan.TargetVersion] == nil {
		l.Entries[plan.TargetVersion] = map[string]entry{}
	}
	l.Entries[plan.TargetVersion][lane.Name] = entry{
		Status:               strings.TrimSpace(status),
		CoveragePolicy:       currentCoveragePolicy,
		RunID:                strings.TrimSpace(runID),
		RunURL:               strings.TrimSpace(runURL),
		Workflow:             strings.TrimSpace(workflow),
		Lane:                 lane.Name,
		ReleaseLine:          plan.ReleaseLine,
		TargetVersion:        plan.TargetVersion,
		PreviousVersion:      plan.PreviousVersion,
		InstallRancher:       lane.InstallRancher,
		UpgradeToRancher:     lane.UpgradeToRancher,
		WebhookChanged:       plan.WebhookChanged,
		WebhookImage:         plan.WebhookImage,
		WebhookOverride:      lane.WebhookOverrideImage,
		PreviousWebhookBuild: plan.PreviousWebhookBuild,
		PreviousWebhookTag:   plan.PreviousWebhookTag,
		TargetWebhookBuild:   plan.TargetWebhookBuild,
		TargetWebhookTag:     plan.TargetWebhookTag,
		SigningPolicy:        plan.SigningPolicy,
		SigningRegistry:      plan.SigningRegistry,
		SigningVerification:  signingResult,
		InstallResolution:    installResolution,
		UpgradeResolution:    upgradeResolution,
		CommitSHA:            strings.TrimSpace(commitSHA),
		CompletedAt:          completedAt,
	}
	if err := writeLedger(ledgerPath, l); err != nil {
		fatalf("write ledger: %v", err)
	}
	fmt.Printf("Recorded %s %s as %s in %s\n", plan.TargetVersion, lane.Name, status, ledgerPath)
}

func readRancherResolution(path string) (*rancherResolution, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(data)) == "" {
		return nil, nil
	}
	var result rancherResolution
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func readSigningResult(path string) (*signingResult, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(data)) == "" {
		return nil, nil
	}
	var result signingResult
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func readPlan(path string) (signoffPlan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return signoffPlan{}, err
	}
	var plan signoffPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return signoffPlan{}, err
	}
	return plan, nil
}

func findLane(plan signoffPlan, laneName string) (signoffLane, error) {
	for _, lane := range plan.Lanes {
		if lane.Name == laneName {
			return lane, nil
		}
	}
	return signoffLane{}, fmt.Errorf("lane %q not found", laneName)
}

func readLedger(path string) (ledger, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ledger{SchemaVersion: ledgerSchemaVersion, Entries: map[string]map[string]entry{}}, nil
	}
	if err != nil {
		return ledger{}, err
	}
	if strings.TrimSpace(string(data)) == "" {
		return ledger{SchemaVersion: ledgerSchemaVersion, Entries: map[string]map[string]entry{}}, nil
	}
	var l ledger
	if err := json.Unmarshal(data, &l); err != nil {
		return ledger{}, err
	}
	return l, nil
}

func writeLedger(path string, l ledger) error {
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil && filepath.Dir(path) != "." {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func fatalf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
