package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	rancherRepo           = "rancher/rancher"
	defaultWebhook        = "rancher/rancher-webhook"
	currentCoveragePolicy = "alpha-webhook-signoff-v2"
	laneFreshAlpha        = "fresh-alpha"
	laneUpgradeAlpha      = "upgrade-alpha"
	laneOldWebhook        = "previous-with-candidate-webhook"
	laneLocalSuites       = "fresh-alpha-local-suites"
)

var (
	alphaVersionRE   = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)-alpha(\d+)$`)
	releaseVersionRE = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)$`)
	webhookBuildRE   = regexp.MustCompile(`(?m)^\s*webhookVersion:\s*["']?([^"'\s]+)["']?\s*$`)
)

type plan struct {
	TargetVersion        string        `json:"target_version"`
	ReleaseLine          string        `json:"release_line"`
	PreviousVersion      string        `json:"previous_version"`
	TargetWebhookBuild   string        `json:"target_webhook_build"`
	TargetWebhookTag     string        `json:"target_webhook_tag"`
	PreviousWebhookBuild string        `json:"previous_webhook_build"`
	PreviousWebhookTag   string        `json:"previous_webhook_tag"`
	WebhookChanged       bool          `json:"webhook_changed"`
	WebhookImage         string        `json:"webhook_image"`
	SigningPolicyInput   string        `json:"signing_policy_input"`
	SigningPolicy        string        `json:"signing_policy"`
	SigningRegistry      string        `json:"signing_registry"`
	RunID                string        `json:"run_id,omitempty"`
	StateKeyRoot         string        `json:"state_key_root,omitempty"`
	Ignored              bool          `json:"ignored,omitempty"`
	IgnoreReason         string        `json:"ignore_reason,omitempty"`
	Lanes                []lane        `json:"lanes"`
	SkippedLanes         []skippedLane `json:"skipped_lanes,omitempty"`
	GeneratedAt          string        `json:"generated_at"`
}

type planSet struct {
	Mode        string `json:"mode"`
	MaxAgeDays  int    `json:"max_age_days"`
	Plans       []plan `json:"plans"`
	GeneratedAt string `json:"generated_at"`
}

type lane struct {
	Name                 string `json:"name"`
	InstallRancher       string `json:"install_rancher"`
	UpgradeToRancher     string `json:"upgrade_to_rancher,omitempty"`
	ProvisionDownstream  bool   `json:"provision_downstream"`
	WebhookOverrideImage string `json:"webhook_override_image,omitempty"`
	TerraformStateKey    string `json:"terraform_state_key,omitempty"`
	AWSPrefix            string `json:"aws_prefix,omitempty"`
	Description          string `json:"description"`
}

type skippedLane struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

type signoffLedger struct {
	Entries map[string]map[string]ledgerEntry `json:"entries"`
}

type targetIgnoreList struct {
	Versions     map[string]string `json:"versions"`
	ReleaseLines map[string]string `json:"release_lines"`
}

type ledgerEntry struct {
	Status         string `json:"status"`
	CoveragePolicy string `json:"coverage_policy"`
	RunID          string `json:"run_id"`
	CompletedAt    string `json:"completed_at"`
}

type semver struct {
	Major int
	Minor int
	Patch int
	Alpha int
	Raw   string
}

type release struct {
	TagName     string `json:"tag_name"`
	Prerelease  bool   `json:"prerelease"`
	PublishedAt string `json:"published_at"`
}

type githubClient struct {
	token            string
	httpClient       *http.Client
	apiBaseURL       string
	rawBaseURL       string
	registryBaseURLs map[string]string
}

func main() {
	var targetVersion string
	var previousVersion string
	var webhookImage string
	var signingPolicy string
	var outputPath string
	var runID string
	var stateKeyRoot string
	var ledgerPath string
	var ignoreListPath string
	var latestAlpha bool
	var latestAlphaPerLine bool
	var ignoreLedger bool
	var maxAgeDays int

	flag.StringVar(&targetVersion, "rancher-version", "", "target Rancher alpha tag, for example v2.14.1-alpha6")
	flag.StringVar(&previousVersion, "previous-rancher-version", "", "previous Rancher release tag; resolved automatically when omitted")
	flag.StringVar(&webhookImage, "webhook-image", "", "candidate webhook image; when omitted, probes staging SUSE, Prime, public SUSE, then Docker Hub for the target webhook tag")
	flag.StringVar(&signingPolicy, "signing-policy", "auto", "required, report-only, skip, or auto")
	flag.StringVar(&outputPath, "output", "", "optional JSON output path")
	flag.StringVar(&runID, "run-id", os.Getenv("GITHUB_RUN_ID"), "workflow run id used to generate per-lane Terraform state keys")
	flag.StringVar(&stateKeyRoot, "state-key-root", "ha-rancher-rke2/signoff", "root prefix for generated Terraform state keys")
	flag.StringVar(&ledgerPath, "ledger", "signoff-ledger.json", "sign-off ledger path used to skip already successful lanes")
	flag.StringVar(&ignoreListPath, "ignore-list", "signoff-ignore.json", "target ignore list path used to skip known-bad versions or release lines")
	flag.BoolVar(&latestAlpha, "latest-alpha", false, "resolve the latest Rancher alpha from GitHub releases")
	flag.BoolVar(&latestAlphaPerLine, "latest-alpha-per-line", false, "resolve the latest Rancher alpha per vX.Y release line from GitHub releases")
	flag.BoolVar(&ignoreLedger, "ignore-ledger", false, "ignore sign-off ledger entries when rendering lanes")
	flag.IntVar(&maxAgeDays, "max-age-days", 30, "maximum alpha release age in days for -latest-alpha-per-line")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	client := githubClient{
		token: os.Getenv("GH_TOKEN"),
		httpClient: &http.Client{
			Timeout: 20 * time.Second,
		},
	}
	ledger, err := readLedger(ledgerPath)
	if err != nil {
		fatalf("read ledger: %v", err)
	}
	ignoreList, err := readTargetIgnoreList(ignoreListPath)
	if err != nil {
		fatalf("read ignore list: %v", err)
	}

	if latestAlpha && latestAlphaPerLine {
		fatalf("set only one of -latest-alpha or -latest-alpha-per-line")
	}

	if latestAlphaPerLine {
		targets, err := client.latestAlphasPerLine(ctx, time.Duration(maxAgeDays)*24*time.Hour)
		if err != nil {
			fatalf("resolve latest alpha per line: %v", err)
		}
		plans := make([]plan, 0, len(targets))
		for _, version := range targets {
			if reason, ignored := ignoreList.reasonFor(version); ignored {
				p, err := ignoredPlan(version, reason, runID, stateKeyRoot)
				if err != nil {
					fatalf("build ignored sign-off plan for %s: %v", version, err)
				}
				plans = append(plans, p)
				continue
			}
			p, err := buildPlan(ctx, client, version, previousVersion, webhookImage, signingPolicy, runID, stateKeyRoot)
			if err != nil {
				fatalf("build sign-off plan for %s: %v", version, err)
			}
			if !ignoreLedger {
				p = applyLedgerSkips(p, ledger)
			}
			plans = append(plans, p)
		}
		writeJSON(planSet{
			Mode:        "latest-alpha-per-line",
			MaxAgeDays:  maxAgeDays,
			Plans:       plans,
			GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		}, outputPath)
		return
	}

	if latestAlpha {
		version, err := client.latestAlpha(ctx)
		if err != nil {
			fatalf("resolve latest alpha: %v", err)
		}
		targetVersion = version
	}

	if targetVersion == "" {
		fatalf("set -rancher-version, -latest-alpha, or -latest-alpha-per-line")
	}

	if reason, ignored := ignoreList.reasonFor(targetVersion); ignored {
		p, err := ignoredPlan(targetVersion, reason, runID, stateKeyRoot)
		if err != nil {
			fatalf("build ignored sign-off plan: %v", err)
		}
		writeJSON(p, outputPath)
		return
	}

	p, err := buildPlan(ctx, client, targetVersion, previousVersion, webhookImage, signingPolicy, runID, stateKeyRoot)
	if err != nil {
		fatalf("build sign-off plan: %v", err)
	}
	if !ignoreLedger {
		p = applyLedgerSkips(p, ledger)
	}

	writeJSON(p, outputPath)
}

func writeJSON(value interface{}, outputPath string) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		fatalf("render plan JSON: %v", err)
	}
	data = append(data, '\n')

	if outputPath != "" {
		if err := os.WriteFile(outputPath, data, 0o644); err != nil {
			fatalf("write %s: %v", outputPath, err)
		}
	}

	if _, err := os.Stdout.Write(data); err != nil {
		fatalf("write stdout: %v", err)
	}
}

func buildPlan(ctx context.Context, client githubClient, targetVersion, previousVersion, webhookImage, signingPolicyInput, runID, stateKeyRoot string) (plan, error) {
	target, err := parseAlphaVersion(targetVersion)
	if err != nil {
		return plan{}, err
	}
	targetVersion = normalizeTag(targetVersion)

	if previousVersion == "" {
		resolved, err := client.resolvePreviousRelease(ctx, target)
		if err != nil {
			return plan{}, err
		}
		previousVersion = resolved
	}
	previousVersion = normalizeTag(previousVersion)
	if _, err := parseReleaseVersion(previousVersion); err != nil {
		return plan{}, fmt.Errorf("previous Rancher version must be a release tag like v2.14.0: %w", err)
	}

	targetBuild, err := client.webhookBuild(ctx, targetVersion)
	if err != nil {
		return plan{}, fmt.Errorf("target %s: %w", targetVersion, err)
	}
	previousBuild, err := client.webhookBuild(ctx, previousVersion)
	if err != nil {
		return plan{}, fmt.Errorf("previous %s: %w", previousVersion, err)
	}

	targetWebhookTag, err := webhookTagFromBuild(targetBuild)
	if err != nil {
		return plan{}, fmt.Errorf("target webhook build %q: %w", targetBuild, err)
	}
	previousWebhookTag, err := webhookTagFromBuild(previousBuild)
	if err != nil {
		return plan{}, fmt.Errorf("previous webhook build %q: %w", previousBuild, err)
	}

	if strings.TrimSpace(webhookImage) == "" {
		resolvedImage, err := client.resolveWebhookImage(ctx, targetWebhookTag)
		if err != nil {
			return plan{}, err
		}
		webhookImage = resolvedImage
	}
	registry, _, _, err := parseImage(webhookImage)
	if err != nil {
		return plan{}, err
	}
	if err := client.validateWebhookImage(ctx, webhookImage, targetWebhookTag); err != nil {
		return plan{}, err
	}
	resolvedPolicy, err := resolveSigningPolicy(signingPolicyInput, registry)
	if err != nil {
		return plan{}, err
	}

	webhookChanged := targetWebhookTag != previousWebhookTag
	lanes := []lane{
		{
			Name:                laneFreshAlpha,
			InstallRancher:      targetVersion,
			ProvisionDownstream: true,
			Description:         fmt.Sprintf("Fresh install %s, provision downstream Linode, run webhook suite.", targetVersion),
		},
		{
			Name:                laneUpgradeAlpha,
			InstallRancher:      previousVersion,
			UpgradeToRancher:    targetVersion,
			ProvisionDownstream: true,
			Description:         fmt.Sprintf("Install %s, provision downstream Linode, upgrade to %s, run webhook suite.", previousVersion, targetVersion),
		},
		{
			Name:                laneLocalSuites,
			InstallRancher:      targetVersion,
			ProvisionDownstream: false,
			Description:         fmt.Sprintf("Fresh install %s, run local-cluster suites such as frameworks regression and VAI enabled.", targetVersion),
		},
	}

	var skipped []skippedLane
	if webhookChanged {
		lanes = append(lanes, lane{
			Name:                 laneOldWebhook,
			InstallRancher:       previousVersion,
			ProvisionDownstream:  true,
			WebhookOverrideImage: webhookImage,
			Description:          fmt.Sprintf("Install %s, provision downstream Linode, override local and downstream webhook to %s, run webhook suite.", previousVersion, webhookImage),
		})
	} else {
		skipped = append(skipped, skippedLane{
			Name:   laneOldWebhook,
			Reason: fmt.Sprintf("Target alpha reuses previous Rancher webhook tag %s; overriding the old Rancher to the same webhook adds no coverage.", targetWebhookTag),
		})
	}
	applyLaneRuntimeFields(lanes, targetVersion, fmt.Sprintf("v%d.%d", target.Major, target.Minor), runID, stateKeyRoot)

	return plan{
		TargetVersion:        targetVersion,
		ReleaseLine:          fmt.Sprintf("v%d.%d", target.Major, target.Minor),
		PreviousVersion:      previousVersion,
		TargetWebhookBuild:   targetBuild,
		TargetWebhookTag:     targetWebhookTag,
		PreviousWebhookBuild: previousBuild,
		PreviousWebhookTag:   previousWebhookTag,
		WebhookChanged:       webhookChanged,
		WebhookImage:         webhookImage,
		SigningPolicyInput:   normalizePolicyInput(signingPolicyInput),
		SigningPolicy:        resolvedPolicy,
		SigningRegistry:      registry,
		RunID:                strings.TrimSpace(runID),
		StateKeyRoot:         strings.Trim(strings.TrimSpace(stateKeyRoot), "/"),
		Lanes:                lanes,
		SkippedLanes:         skipped,
		GeneratedAt:          time.Now().UTC().Format(time.RFC3339),
	}, nil
}

func readLedger(path string) (signoffLedger, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return signoffLedger{}, nil
	}
	if err != nil {
		return signoffLedger{}, err
	}
	if strings.TrimSpace(string(data)) == "" {
		return signoffLedger{}, nil
	}
	var ledger signoffLedger
	if err := json.Unmarshal(data, &ledger); err != nil {
		return signoffLedger{}, err
	}
	return ledger, nil
}

func readTargetIgnoreList(path string) (targetIgnoreList, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return targetIgnoreList{}, nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return targetIgnoreList{}, nil
	}
	if err != nil {
		return targetIgnoreList{}, err
	}
	if strings.TrimSpace(string(data)) == "" {
		return targetIgnoreList{}, nil
	}
	var ignoreList targetIgnoreList
	if err := json.Unmarshal(data, &ignoreList); err != nil {
		return targetIgnoreList{}, err
	}
	return normalizeTargetIgnoreList(ignoreList), nil
}

func normalizeTargetIgnoreList(ignoreList targetIgnoreList) targetIgnoreList {
	normalized := targetIgnoreList{}
	if len(ignoreList.Versions) > 0 {
		normalized.Versions = map[string]string{}
		for version, reason := range ignoreList.Versions {
			version = strings.TrimSpace(version)
			if version == "" {
				continue
			}
			version = normalizeTag(version)
			reason = strings.TrimSpace(reason)
			if version != "" && reason != "" {
				normalized.Versions[version] = reason
			}
		}
	}
	if len(ignoreList.ReleaseLines) > 0 {
		normalized.ReleaseLines = map[string]string{}
		for releaseLine, reason := range ignoreList.ReleaseLines {
			releaseLine = normalizeReleaseLine(releaseLine)
			reason = strings.TrimSpace(reason)
			if releaseLine != "" && reason != "" {
				normalized.ReleaseLines[releaseLine] = reason
			}
		}
	}
	return normalized
}

func (ignoreList targetIgnoreList) reasonFor(targetVersion string) (string, bool) {
	if strings.TrimSpace(targetVersion) == "" {
		return "", false
	}
	targetVersion = normalizeTag(targetVersion)
	if reason := ignoreList.Versions[targetVersion]; reason != "" {
		return reason, true
	}
	target, err := parseAlphaVersion(targetVersion)
	if err != nil {
		return "", false
	}
	releaseLine := fmt.Sprintf("v%d.%d", target.Major, target.Minor)
	if reason := ignoreList.ReleaseLines[releaseLine]; reason != "" {
		return reason, true
	}
	return "", false
}

func ignoredPlan(targetVersion, reason, runID, stateKeyRoot string) (plan, error) {
	target, err := parseAlphaVersion(targetVersion)
	if err != nil {
		return plan{}, err
	}
	targetVersion = normalizeTag(targetVersion)
	releaseLine := fmt.Sprintf("v%d.%d", target.Major, target.Minor)
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "Target is listed in the repo-owned sign-off ignore list."
	}
	return plan{
		TargetVersion: targetVersion,
		ReleaseLine:   releaseLine,
		RunID:         strings.TrimSpace(runID),
		StateKeyRoot:  strings.Trim(strings.TrimSpace(stateKeyRoot), "/"),
		Ignored:       true,
		IgnoreReason:  reason,
		Lanes:         []lane{},
		SkippedLanes: []skippedLane{
			{Name: laneFreshAlpha, Reason: reason},
			{Name: laneUpgradeAlpha, Reason: reason},
			{Name: laneLocalSuites, Reason: reason},
			{Name: laneOldWebhook, Reason: reason},
		},
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
	}, nil
}

func applyLedgerSkips(p plan, ledger signoffLedger) plan {
	if ledger.Entries == nil {
		return p
	}
	byLane := ledger.Entries[p.TargetVersion]
	if len(byLane) == 0 {
		return p
	}
	remaining := make([]lane, 0, len(p.Lanes))
	for _, lane := range p.Lanes {
		entry, ok := byLane[lane.Name]
		if !ok || entry.Status != "success" || entry.CoveragePolicy != currentCoveragePolicy {
			remaining = append(remaining, lane)
			continue
		}
		reason := fmt.Sprintf("Already recorded successful sign-off for %s/%s", p.TargetVersion, lane.Name)
		var details []string
		if entry.RunID != "" {
			details = append(details, "run "+entry.RunID)
		}
		if entry.CompletedAt != "" {
			details = append(details, "completed "+entry.CompletedAt)
		}
		if len(details) > 0 {
			reason += " (" + strings.Join(details, ", ") + ")"
		}
		p.SkippedLanes = append(p.SkippedLanes, skippedLane{Name: lane.Name, Reason: reason})
	}
	p.Lanes = remaining
	return p
}

func applyLaneRuntimeFields(lanes []lane, targetVersion, releaseLine, runID, stateKeyRoot string) {
	runID = strings.TrimSpace(runID)
	stateKeyRoot = strings.Trim(strings.TrimSpace(stateKeyRoot), "/")
	for i := range lanes {
		lanes[i].AWSPrefix = buildLaneAWSPrefix(runID, lanes[i].Name)
		if runID != "" && stateKeyRoot != "" {
			lanes[i].TerraformStateKey = buildTerraformStateKey(stateKeyRoot, releaseLine, targetVersion, runID, lanes[i].Name)
		}
	}
}

func buildTerraformStateKey(root, releaseLine, targetVersion, runID, laneName string) string {
	parts := []string{
		strings.Trim(root, "/"),
		sanitizeStateKeyPart(releaseLine),
		sanitizeStateKeyPart(targetVersion),
		sanitizeStateKeyPart(runID),
		sanitizeStateKeyPart(laneName),
		"terraform.tfstate",
	}
	return strings.Join(parts, "/")
}

func buildLaneAWSPrefix(runID, laneName string) string {
	laneCode := map[string]string{
		laneFreshAlpha:   "fa",
		laneUpgradeAlpha: "ua",
		laneOldWebhook:   "ow",
		laneLocalSuites:  "ls",
	}[laneName]
	if laneCode == "" {
		laneCode = "ln"
	}
	runID = compactRunID(runID)
	if runID == "" {
		return "local-" + laneCode
	}
	return "gha-" + runID + "-" + laneCode
}

func compactRunID(runID string) string {
	runID = sanitizeAWSNamePart(runID)
	if len(runID) <= 8 {
		return runID
	}
	return runID[len(runID)-8:]
}

func sanitizeStateKeyPart(value string) string {
	value = strings.TrimSpace(value)
	value = strings.Trim(value, "/")
	value = strings.ReplaceAll(value, " ", "-")
	if value == "" {
		return "unknown"
	}
	return value
}

func sanitizeAWSNamePart(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	return strings.Trim(b.String(), "-")
}

func normalizeReleaseLine(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, "v") {
		return value
	}
	return "v" + value
}

func (c githubClient) latestAlpha(ctx context.Context) (string, error) {
	releases, err := c.releases(ctx)
	if err != nil {
		return "", err
	}

	for _, release := range releases {
		if release.Prerelease && alphaVersionRE.MatchString(release.TagName) {
			return normalizeTag(release.TagName), nil
		}
	}

	return "", fmt.Errorf("no alpha release found in recent %s releases", rancherRepo)
}

func (c githubClient) latestAlphasPerLine(ctx context.Context, maxAge time.Duration) ([]string, error) {
	releases, err := c.releases(ctx)
	if err != nil {
		return nil, err
	}

	cutoff := time.Now().UTC().Add(-maxAge)
	targets := latestAlphasPerLineFromReleases(releases, cutoff)
	if len(targets) == 0 {
		return nil, fmt.Errorf("no alpha release found in recent %s releases", rancherRepo)
	}
	return targets, nil
}

func latestAlphasPerLineFromReleases(releases []release, cutoff time.Time) []string {
	byLine := map[string]semver{}
	for _, release := range releases {
		if !release.Prerelease {
			continue
		}
		parsed, err := parseAlphaVersion(release.TagName)
		if err != nil {
			continue
		}
		if release.PublishedAt != "" {
			publishedAt, err := time.Parse(time.RFC3339, release.PublishedAt)
			if err != nil {
				continue
			}
			if publishedAt.Before(cutoff) {
				continue
			}
		}
		line := fmt.Sprintf("v%d.%d", parsed.Major, parsed.Minor)
		if current, ok := byLine[line]; !ok || semverLess(current, parsed) {
			byLine[line] = parsed
		}
	}
	if len(byLine) == 0 {
		return nil
	}

	lines := make([]string, 0, len(byLine))
	for line := range byLine {
		lines = append(lines, line)
	}
	sort.Slice(lines, func(i, j int) bool {
		return semverLess(byLine[lines[j]], byLine[lines[i]])
	})

	targets := make([]string, 0, len(lines))
	for _, line := range lines {
		targets = append(targets, normalizeTag(byLine[line].Raw))
	}
	return targets
}

func (c githubClient) resolvePreviousRelease(ctx context.Context, target semver) (string, error) {
	if target.Patch > 0 {
		candidate := fmt.Sprintf("v%d.%d.%d", target.Major, target.Minor, target.Patch-1)
		if ok, err := c.releaseExists(ctx, candidate); err != nil {
			return "", err
		} else if ok {
			return candidate, nil
		}
	}

	releases, err := c.releases(ctx)
	if err != nil {
		return "", err
	}

	var candidates []semver
	for _, release := range releases {
		parsed, err := parseReleaseVersion(release.TagName)
		if err != nil {
			continue
		}
		if parsed.Major != target.Major {
			continue
		}
		if parsed.Minor > target.Minor || (parsed.Minor == target.Minor && parsed.Patch >= target.Patch) {
			continue
		}
		candidates = append(candidates, parsed)
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("no previous Rancher release found for %s", target.Raw)
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Minor != candidates[j].Minor {
			return candidates[i].Minor > candidates[j].Minor
		}
		return candidates[i].Patch > candidates[j].Patch
	})

	return normalizeTag(candidates[0].Raw), nil
}

func semverLess(a, b semver) bool {
	if a.Major != b.Major {
		return a.Major < b.Major
	}
	if a.Minor != b.Minor {
		return a.Minor < b.Minor
	}
	if a.Patch != b.Patch {
		return a.Patch < b.Patch
	}
	return a.Alpha < b.Alpha
}

func (c githubClient) releaseExists(ctx context.Context, tag string) (bool, error) {
	url := fmt.Sprintf("%s/repos/%s/releases/tags/%s", c.apiBase(), rancherRepo, tag)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	c.addHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return false, fmt.Errorf("GitHub release lookup failed for %s: %s: %s", tag, resp.Status, strings.TrimSpace(string(body)))
	}
	return true, nil
}

func (c githubClient) releases(ctx context.Context) ([]release, error) {
	url := fmt.Sprintf("%s/repos/%s/releases?per_page=100", c.apiBase(), rancherRepo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	c.addHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("GitHub releases request failed: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var releases []release
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return nil, err
	}
	return releases, nil
}

func (c githubClient) webhookBuild(ctx context.Context, tag string) (string, error) {
	url := fmt.Sprintf("%s/%s/%s/build.yaml", c.rawBase(), rancherRepo, tag)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	c.addHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("build.yaml request failed: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if err != nil {
		return "", err
	}
	return parseWebhookBuild(string(body))
}

func (c githubClient) resolveWebhookImage(ctx context.Context, tag string) (string, error) {
	var failures []string
	for _, repository := range webhookImageCandidates(tag) {
		image := repository + ":" + tag
		registry, repo, _, err := parseImage(image)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", image, err))
			continue
		}
		found, err := c.registryImageTagExists(ctx, registry, repo, tag)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", image, err))
			continue
		}
		if found {
			return image, nil
		}
		failures = append(failures, image+": tag not found")
	}

	return "", fmt.Errorf("webhook image tag %s was not found in candidate registries: %s", tag, strings.Join(failures, "; "))
}

func webhookImageCandidates(tag string) []string {
	if isPrereleaseWebhookTag(tag) {
		return []string{
			"stgregistry.suse.com/" + defaultWebhook,
			"registry.rancher.com/" + defaultWebhook,
			"registry.suse.com/" + defaultWebhook,
			"docker.io/" + defaultWebhook,
		}
	}
	return []string{
		"registry.suse.com/" + defaultWebhook,
		"registry.rancher.com/" + defaultWebhook,
		"stgregistry.suse.com/" + defaultWebhook,
		"docker.io/" + defaultWebhook,
	}
}

func isPrereleaseWebhookTag(tag string) bool {
	tag = strings.TrimSpace(tag)
	tag = strings.TrimPrefix(tag, "v")
	return strings.Contains(tag, "-")
}

func (c githubClient) validateWebhookImage(ctx context.Context, image, expectedTag string) error {
	registry, repository, tag, err := parseImage(image)
	if err != nil {
		return err
	}
	if tag != expectedTag {
		return fmt.Errorf("webhook image %s uses tag %s, expected %s from Rancher build.yaml", image, tag, expectedTag)
	}
	found, err := c.registryImageTagExists(ctx, registry, repository, tag)
	if err != nil {
		return fmt.Errorf("validate webhook image %s: %w", image, err)
	}
	if !found {
		return fmt.Errorf("webhook image %s was not found in registry", image)
	}
	return nil
}

func (c githubClient) registryImageTagExists(ctx context.Context, registry, repository, tag string) (bool, error) {
	url := fmt.Sprintf("%s/v2/%s/manifests/%s", c.registryBase(registry), repository, tag)
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.docker.distribution.manifest.v2+json",
	}, ", "))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	case http.StatusUnauthorized:
		token, err := c.registryBearerToken(ctx, resp.Header.Get("WWW-Authenticate"))
		if err != nil {
			return false, err
		}
		return c.registryImageTagExistsWithToken(ctx, url, token)
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return false, fmt.Errorf("registry manifest lookup failed: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
}

func (c githubClient) registryImageTagExistsWithToken(ctx context.Context, manifestURL, token string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, manifestURL, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.docker.distribution.manifest.v2+json",
	}, ", "))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return false, fmt.Errorf("registry authenticated manifest lookup failed: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
}

func (c githubClient) registryBearerToken(ctx context.Context, authenticate string) (string, error) {
	params, err := parseBearerChallenge(authenticate)
	if err != nil {
		return "", err
	}
	realm := params["realm"]
	if realm == "" {
		return "", fmt.Errorf("registry Bearer challenge missing realm")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm, nil)
	if err != nil {
		return "", err
	}
	query := req.URL.Query()
	if service := params["service"]; service != "" {
		query.Set("service", service)
	}
	if scope := params["scope"]; scope != "" {
		query.Set("scope", scope)
	}
	req.URL.RawQuery = query.Encode()

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("registry token request failed: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var tokenResponse struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResponse); err != nil {
		return "", err
	}
	token := tokenResponse.Token
	if token == "" {
		token = tokenResponse.AccessToken
	}
	if token == "" {
		return "", fmt.Errorf("registry token response did not include a token")
	}
	return token, nil
}

func parseBearerChallenge(value string) (map[string]string, error) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(strings.ToLower(value), "bearer ") {
		return nil, fmt.Errorf("unsupported registry auth challenge %q", value)
	}
	value = strings.TrimSpace(value[len("Bearer "):])
	params := map[string]string{}
	for _, part := range strings.Split(value, ",") {
		key, rawValue, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		params[strings.ToLower(strings.TrimSpace(key))] = strings.Trim(strings.TrimSpace(rawValue), `"`)
	}
	return params, nil
}

func (c githubClient) addHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "ha-rancher-rke2-signoff-plan")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

func (c githubClient) apiBase() string {
	if c.apiBaseURL != "" {
		return strings.TrimRight(c.apiBaseURL, "/")
	}
	return "https://api.github.com"
}

func (c githubClient) rawBase() string {
	if c.rawBaseURL != "" {
		return strings.TrimRight(c.rawBaseURL, "/")
	}
	return "https://raw.githubusercontent.com"
}

func (c githubClient) registryBase(registry string) string {
	if c.registryBaseURLs != nil {
		if base := c.registryBaseURLs[registry]; base != "" {
			return strings.TrimRight(base, "/")
		}
	}
	if registry == "docker.io" {
		return "https://registry-1.docker.io"
	}
	return "https://" + registry
}

func parseWebhookBuild(buildYAML string) (string, error) {
	match := webhookBuildRE.FindStringSubmatch(buildYAML)
	if len(match) != 2 {
		return "", errors.New("webhookVersion not found in build.yaml")
	}
	return strings.TrimSpace(match[1]), nil
}

func webhookTagFromBuild(build string) (string, error) {
	build = strings.TrimSpace(build)
	if build == "" {
		return "", errors.New("empty webhook build")
	}
	idx := strings.LastIndex(build, "+up")
	if idx < 0 || idx+3 >= len(build) {
		return "", fmt.Errorf("expected build format like 109.0.1+up0.10.1-rc.5")
	}
	version := strings.TrimSpace(build[idx+3:])
	if version == "" {
		return "", fmt.Errorf("empty +up version in %q", build)
	}
	return "v" + strings.TrimPrefix(version, "v"), nil
}

func parseAlphaVersion(value string) (semver, error) {
	match := alphaVersionRE.FindStringSubmatch(strings.TrimSpace(value))
	if len(match) != 5 {
		return semver{}, fmt.Errorf("target Rancher version must be an alpha tag like v2.14.1-alpha6")
	}
	return parseVersionParts(value, match)
}

func parseReleaseVersion(value string) (semver, error) {
	match := releaseVersionRE.FindStringSubmatch(strings.TrimSpace(value))
	if len(match) != 4 {
		return semver{}, fmt.Errorf("not a release version: %s", value)
	}
	return parseVersionParts(value, match)
}

func parseVersionParts(raw string, match []string) (semver, error) {
	major, err := strconv.Atoi(match[1])
	if err != nil {
		return semver{}, err
	}
	minor, err := strconv.Atoi(match[2])
	if err != nil {
		return semver{}, err
	}
	patch, err := strconv.Atoi(match[3])
	if err != nil {
		return semver{}, err
	}
	alpha := 0
	if len(match) > 4 {
		alpha, err = strconv.Atoi(match[4])
		if err != nil {
			return semver{}, err
		}
	}
	return semver{Major: major, Minor: minor, Patch: patch, Alpha: alpha, Raw: normalizeTag(raw)}, nil
}

func parseImage(image string) (registry, repository, tag string, err error) {
	image = strings.TrimSpace(image)
	if image == "" {
		return "", "", "", errors.New("webhook image must not be empty")
	}

	slash := strings.IndexByte(image, '/')
	if slash < 0 {
		return "", "", "", fmt.Errorf("webhook image must include a registry and repository: %s", image)
	}
	registry = image[:slash]
	remainder := image[slash+1:]
	colon := strings.LastIndexByte(remainder, ':')
	if colon < 0 || colon == len(remainder)-1 {
		return "", "", "", fmt.Errorf("webhook image must include a tag: %s", image)
	}
	repository = remainder[:colon]
	tag = remainder[colon+1:]
	if registry == "" || repository == "" || tag == "" {
		return "", "", "", fmt.Errorf("invalid webhook image: %s", image)
	}
	return registry, repository, tag, nil
}

func resolveSigningPolicy(input, registry string) (string, error) {
	input = normalizePolicyInput(input)
	switch input {
	case "required", "report-only", "skip":
		return input, nil
	case "auto":
		return "report-only", nil
	default:
		return "", fmt.Errorf("unsupported signing policy %q", input)
	}
}

func normalizePolicyInput(input string) string {
	input = strings.ToLower(strings.TrimSpace(input))
	if input == "" {
		return "auto"
	}
	return input
}

func normalizeTag(version string) string {
	version = strings.TrimSpace(version)
	return "v" + strings.TrimPrefix(version, "v")
}

func fatalf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
