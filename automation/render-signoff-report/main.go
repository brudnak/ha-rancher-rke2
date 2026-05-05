package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	SigningPolicy      string        `json:"signing_policy"`
	SigningRegistry    string        `json:"signing_registry"`
	Lanes              []signoffLane `json:"lanes"`
	SkippedLanes       []skippedLane `json:"skipped_lanes"`
}

type signoffLane struct {
	Name                 string `json:"name"`
	InstallRancher       string `json:"install_rancher"`
	UpgradeToRancher     string `json:"upgrade_to_rancher"`
	ProvisionDownstream  bool   `json:"provision_downstream"`
	WebhookOverrideImage string `json:"webhook_override_image"`
	TerraformStateKey    string `json:"terraform_state_key"`
	AWSPrefix            string `json:"aws_prefix"`
	Description          string `json:"description"`
}

type skippedLane struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

type metadata map[string]interface{}

func main() {
	var planPath string
	var outputPath string
	var outputDir string
	var laneName string

	flag.StringVar(&planPath, "plan", "signoff-plan.json", "sign-off plan JSON path")
	flag.StringVar(&outputPath, "output", filepath.Join("automation-output", "signoff-report.md"), "Markdown report output path")
	flag.StringVar(&outputDir, "output-dir", "automation-output", "directory containing lane metadata JSON files")
	flag.StringVar(&laneName, "lane", "", "optional active lane name")
	flag.Parse()

	plan, err := readPlan(planPath)
	if err != nil {
		fatalf("read plan: %v", err)
	}
	report, err := renderReport(plan, outputDir, laneName, time.Now().UTC())
	if err != nil {
		fatalf("render report: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		fatalf("create report dir: %v", err)
	}
	if err := os.WriteFile(outputPath, []byte(report), 0o644); err != nil {
		fatalf("write report: %v", err)
	}
	fmt.Print(report)
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

func renderReport(plan signoffPlan, outputDir, activeLane string, generatedAt time.Time) (string, error) {
	downstream, err := readMetadataFiles(filepath.Join(outputDir, "downstream-ha-*.json"))
	if err != nil {
		return "", err
	}
	localSuites, err := readMetadataFiles(filepath.Join(outputDir, "local-suite-ha-*.json"))
	if err != nil {
		return "", err
	}
	overrides, err := readMetadataFiles(filepath.Join(outputDir, "webhook-override-*.json"))
	if err != nil {
		return "", err
	}
	rancherTestRuns, err := readMetadataFiles(filepath.Join(outputDir, "rancher-test-results.json"))
	if err != nil {
		return "", err
	}
	signingRuns, err := readMetadataFiles(filepath.Join(outputDir, "webhook-signing.json"))
	if err != nil {
		return "", err
	}
	rancherTests := expandRancherTestRows(rancherTestRuns)

	var b strings.Builder
	fmt.Fprintf(&b, "# %s Sign-Off Report\n\n", valueOr(plan.TargetVersion, "Rancher Alpha"))
	fmt.Fprintf(&b, "Generated: `%s`\n\n", generatedAt.Format(time.RFC3339))
	if activeLane != "" {
		fmt.Fprintf(&b, "Active lane: `%s`\n\n", activeLane)
	}

	fmt.Fprintf(&b, "## Plan\n\n")
	fmt.Fprintf(&b, "- Target Rancher: `%s`\n", plan.TargetVersion)
	fmt.Fprintf(&b, "- Previous Rancher: `%s`\n", plan.PreviousVersion)
	fmt.Fprintf(&b, "- Webhook image: `%s`\n", plan.WebhookImage)
	fmt.Fprintf(&b, "- Webhook changed: `%t` (`%s` -> `%s`)\n", plan.WebhookChanged, plan.PreviousWebhookTag, plan.TargetWebhookTag)
	fmt.Fprintf(&b, "- Signing policy: `%s` for `%s`\n\n", plan.SigningPolicy, plan.SigningRegistry)

	fmt.Fprintf(&b, "## Lanes\n\n")
	fmt.Fprintf(&b, "| Lane | Install | Upgrade | Downstream | Webhook override |\n")
	fmt.Fprintf(&b, "| --- | --- | --- | --- | --- |\n")
	for _, lane := range plan.Lanes {
		fmt.Fprintf(&b, "| `%s` | `%s` | %s | `%t` | %s |\n",
			lane.Name,
			lane.InstallRancher,
			codeOrDash(lane.UpgradeToRancher),
			lane.ProvisionDownstream,
			codeOrDash(lane.WebhookOverrideImage))
	}
	if len(plan.SkippedLanes) > 0 {
		fmt.Fprintf(&b, "\n## Skipped\n\n")
		for _, skipped := range plan.SkippedLanes {
			fmt.Fprintf(&b, "- `%s`: %s\n", skipped.Name, skipped.Reason)
		}
	}

	writeMetadataTable(&b, "Downstream Linode", downstream, []string{"ha_index", "k3s_version"})
	writeMetadataTable(&b, "Webhook Signing", signingRuns, []string{"target_version", "webhook_image", "signing_policy", "enforced", "signature_verified", "provenance_verified", "sbom_verified", "verification_error"})
	writeMetadataTable(&b, "Local Suite Targets", localSuites, []string{"ha_index"})
	writeMetadataTable(&b, "Webhook Overrides", overrides, []string{"scope", "ha_index", "rollout_complete"})
	writeMetadataTable(&b, "Rancher Test Runs", rancherTestRuns, []string{"ref", "lane", "rancher_version"})
	writeMetadataTable(&b, "Rancher Test Results", rancherTests, []string{"ref", "lane", "suite", "package", "test_run", "junit", "conclusion"})

	return b.String(), nil
}

func expandRancherTestRows(testRuns []metadata) []metadata {
	var rows []metadata
	for _, testRun := range testRuns {
		results, ok := testRun["results"].([]interface{})
		if !ok {
			continue
		}
		for _, result := range results {
			row := metadata{
				"file": testRun["file"],
				"repo": testRun["repo"],
				"ref":  testRun["ref"],
				"lane": testRun["lane"],
			}
			switch suite := result.(type) {
			case map[string]interface{}:
				for key, value := range suite {
					row[key] = value
				}
			default:
				row["suite"] = suite
			}
			rows = append(rows, row)
		}
	}
	return rows
}

func readMetadataFiles(pattern string) ([]metadata, error) {
	paths, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	items := make([]metadata, 0, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var item metadata
		if err := json.Unmarshal(data, &item); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		item["file"] = filepath.Base(path)
		items = append(items, item)
	}
	return items, nil
}

func writeMetadataTable(b *strings.Builder, title string, rows []metadata, columns []string) {
	fmt.Fprintf(b, "\n## %s\n\n", title)
	if len(rows) == 0 {
		fmt.Fprintf(b, "_No records yet._\n")
		return
	}
	fmt.Fprintf(b, "| File |")
	for _, column := range columns {
		fmt.Fprintf(b, " %s |", header(column))
	}
	fmt.Fprintf(b, "\n| --- |")
	for range columns {
		fmt.Fprintf(b, " --- |")
	}
	fmt.Fprintf(b, "\n")
	for _, row := range rows {
		fmt.Fprintf(b, "| %s |", md(row["file"]))
		for _, column := range columns {
			fmt.Fprintf(b, " %s |", md(row[column]))
		}
		fmt.Fprintf(b, "\n")
	}
}

func header(value string) string {
	return strings.ReplaceAll(strings.Title(strings.ReplaceAll(value, "_", " ")), "Id", "ID")
}

func md(value interface{}) string {
	if value == nil {
		return "-"
	}
	switch v := value.(type) {
	case []interface{}:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			parts = append(parts, fmt.Sprint(item))
		}
		return "`" + escapePipes(strings.Join(parts, ", ")) + "`"
	default:
		text := strings.TrimSpace(fmt.Sprint(v))
		if text == "" {
			return "-"
		}
		return "`" + escapePipes(text) + "`"
	}
}

func codeOrDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return "`" + escapePipes(value) + "`"
}

func valueOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func escapePipes(value string) string {
	return strings.ReplaceAll(value, "|", "\\|")
}

func fatalf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
