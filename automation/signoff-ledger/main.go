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

type signoffPlan struct {
	TargetVersion      string        `json:"target_version"`
	ReleaseLine        string        `json:"release_line"`
	PreviousVersion    string        `json:"previous_version"`
	TargetWebhookTag   string        `json:"target_webhook_tag"`
	PreviousWebhookTag string        `json:"previous_webhook_tag"`
	WebhookChanged     bool          `json:"webhook_changed"`
	WebhookImage       string        `json:"webhook_image"`
	Lanes              []signoffLane `json:"lanes"`
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

type entry struct {
	Status             string `json:"status"`
	RunID              string `json:"run_id"`
	RunURL             string `json:"run_url,omitempty"`
	Workflow           string `json:"workflow,omitempty"`
	Lane               string `json:"lane"`
	ReleaseLine        string `json:"release_line"`
	TargetVersion      string `json:"target_version"`
	PreviousVersion    string `json:"previous_version,omitempty"`
	InstallRancher     string `json:"install_rancher"`
	UpgradeToRancher   string `json:"upgrade_to_rancher,omitempty"`
	WebhookChanged     bool   `json:"webhook_changed"`
	WebhookImage       string `json:"webhook_image,omitempty"`
	WebhookOverride    string `json:"webhook_override_image,omitempty"`
	PreviousWebhookTag string `json:"previous_webhook_tag,omitempty"`
	TargetWebhookTag   string `json:"target_webhook_tag,omitempty"`
	CommitSHA          string `json:"commit_sha,omitempty"`
	CompletedAt        string `json:"completed_at"`
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

	flag.StringVar(&planPath, "plan", "signoff-plan.json", "sign-off plan JSON path")
	flag.StringVar(&ledgerPath, "ledger", "signoff-ledger.json", "sign-off ledger JSON path")
	flag.StringVar(&laneName, "lane", "", "lane name to record")
	flag.StringVar(&status, "status", "success", "lane status")
	flag.StringVar(&runID, "run-id", os.Getenv("GITHUB_RUN_ID"), "GitHub Actions run id")
	flag.StringVar(&runURL, "run-url", "", "GitHub Actions run URL")
	flag.StringVar(&workflow, "workflow", os.Getenv("GITHUB_WORKFLOW"), "GitHub Actions workflow name")
	flag.StringVar(&commitSHA, "commit-sha", os.Getenv("GITHUB_SHA"), "commit SHA tested by the run")
	flag.StringVar(&completedAt, "completed-at", "", "completion time in RFC3339; defaults to now")
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
	if l.SchemaVersion == 0 {
		l.SchemaVersion = 1
	}
	if l.Entries == nil {
		l.Entries = map[string]map[string]entry{}
	}
	if l.Entries[plan.TargetVersion] == nil {
		l.Entries[plan.TargetVersion] = map[string]entry{}
	}
	l.Entries[plan.TargetVersion][lane.Name] = entry{
		Status:             strings.TrimSpace(status),
		RunID:              strings.TrimSpace(runID),
		RunURL:             strings.TrimSpace(runURL),
		Workflow:           strings.TrimSpace(workflow),
		Lane:               lane.Name,
		ReleaseLine:        plan.ReleaseLine,
		TargetVersion:      plan.TargetVersion,
		PreviousVersion:    plan.PreviousVersion,
		InstallRancher:     lane.InstallRancher,
		UpgradeToRancher:   lane.UpgradeToRancher,
		WebhookChanged:     plan.WebhookChanged,
		WebhookImage:       plan.WebhookImage,
		WebhookOverride:    lane.WebhookOverrideImage,
		PreviousWebhookTag: plan.PreviousWebhookTag,
		TargetWebhookTag:   plan.TargetWebhookTag,
		CommitSHA:          strings.TrimSpace(commitSHA),
		CompletedAt:        completedAt,
	}
	if err := writeLedger(ledgerPath, l); err != nil {
		fatalf("write ledger: %v", err)
	}
	fmt.Printf("Recorded %s %s as %s in %s\n", plan.TargetVersion, lane.Name, status, ledgerPath)
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
		return ledger{SchemaVersion: 1, Entries: map[string]map[string]entry{}}, nil
	}
	if err != nil {
		return ledger{}, err
	}
	if strings.TrimSpace(string(data)) == "" {
		return ledger{SchemaVersion: 1, Entries: map[string]map[string]entry{}}, nil
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
