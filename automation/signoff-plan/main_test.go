package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWebhookTagFromBuild(t *testing.T) {
	tag, err := webhookTagFromBuild("109.0.1+up0.10.1-rc.5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tag != "v0.10.1-rc.5" {
		t.Fatalf("expected v0.10.1-rc.5, got %s", tag)
	}
}

func TestParseWebhookBuild(t *testing.T) {
	build, err := parseWebhookBuild(`
defaultShellVersion: rancher/shell:v0.7.0-rc.6
webhookVersion: "109.0.1+up0.10.1-rc.5"
`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if build != "109.0.1+up0.10.1-rc.5" {
		t.Fatalf("unexpected build: %s", build)
	}
}

func TestResolveSigningPolicy(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		registry string
		want     string
	}{
		{name: "suse auto", input: "auto", registry: "registry.suse.com", want: "report-only"},
		{name: "staging auto", input: "auto", registry: "stgregistry.suse.com", want: "report-only"},
		{name: "prime auto", input: "auto", registry: "registry.rancher.com", want: "report-only"},
		{name: "community auto", input: "auto", registry: "docker.io", want: "report-only"},
		{name: "manual required", input: "required", registry: "registry.suse.com", want: "required"},
		{name: "manual skip", input: "skip", registry: "stgregistry.suse.com", want: "skip"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveSigningPolicy(tt.input, tt.registry)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("expected %s, got %s", tt.want, got)
			}
		})
	}
}

func TestWebhookImageCandidatesPreferReleasedSUSERegistryForStableTags(t *testing.T) {
	candidates := webhookImageCandidates("v0.9.3")
	want := "registry.suse.com/rancher/rancher-webhook"
	if len(candidates) == 0 || candidates[0] != want {
		t.Fatalf("expected first stable candidate %s, got %v", want, candidates)
	}
}

func TestWebhookImageCandidatesPreferStagingForPrereleaseTags(t *testing.T) {
	candidates := webhookImageCandidates("v0.10.1-rc.5")
	want := "stgregistry.suse.com/rancher/rancher-webhook"
	if len(candidates) == 0 || candidates[0] != want {
		t.Fatalf("expected first prerelease candidate %s, got %v", want, candidates)
	}
}

func TestBuildPlanAddsOldWebhookLaneWhenWebhookChanged(t *testing.T) {
	client := fakeGitHubClient(t, map[string]string{
		"/rancher/rancher/v2.14.1-alpha6/build.yaml":             `webhookVersion: 109.0.1+up0.10.1-rc.5`,
		"/rancher/rancher/v2.14.0/build.yaml":                    `webhookVersion: 109.0.0+up0.10.0`,
		"/stg/v2/rancher/rancher-webhook/manifests/v0.10.1-rc.5": "ok",
	})

	plan, err := buildPlan(context.Background(), client, "v2.14.1-alpha6", "v2.14.0", "stgregistry.suse.com/rancher/rancher-webhook:v0.10.1-rc.5", "auto", "123456789", "ha-rancher-rke2/signoff")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !plan.WebhookChanged {
		t.Fatal("expected webhook to be marked changed")
	}
	if plan.SigningPolicy != "report-only" {
		t.Fatalf("expected report-only signing policy, got %s", plan.SigningPolicy)
	}
	if len(plan.Lanes) != 4 {
		t.Fatalf("expected 4 lanes, got %d", len(plan.Lanes))
	}
	if plan.Lanes[2].Name != laneLocalSuites {
		t.Fatalf("expected local suites lane, got %s", plan.Lanes[2].Name)
	}
	if plan.Lanes[2].ProvisionDownstream {
		t.Fatal("expected local suites lane to skip downstream provisioning")
	}
	if plan.Lanes[3].Name != laneOldWebhook {
		t.Fatalf("expected old webhook lane, got %s", plan.Lanes[3].Name)
	}
	if plan.Lanes[3].WebhookOverrideImage == "" {
		t.Fatal("expected webhook override image")
	}
	if plan.Lanes[3].TerraformStateKey != "ha-rancher-rke2/signoff/v2.14/v2.14.1-alpha6/123456789/previous-with-candidate-webhook/terraform.tfstate" {
		t.Fatalf("unexpected state key: %s", plan.Lanes[3].TerraformStateKey)
	}
	if plan.Lanes[3].AWSPrefix != "gha-23456789-ow" {
		t.Fatalf("unexpected AWS prefix: %s", plan.Lanes[3].AWSPrefix)
	}
}

func TestBuildPlanDiscoversStagingPrereleaseWebhookImageWhenNoOverride(t *testing.T) {
	client := fakeGitHubClient(t, map[string]string{
		"/rancher/rancher/v2.14.1-alpha6/build.yaml":                `webhookVersion: 109.0.1+up0.10.1-rc.5`,
		"/rancher/rancher/v2.14.0/build.yaml":                       `webhookVersion: 109.0.0+up0.10.0`,
		"/stg/v2/rancher/rancher-webhook/manifests/v0.10.1-rc.5":    "ok",
		"/suse/v2/rancher/rancher-webhook/manifests/v0.10.1-rc.5":   "missing",
		"/docker/v2/rancher/rancher-webhook/manifests/v0.10.1-rc.5": "ok",
	})
	client.registryBaseURLs = map[string]string{
		"stgregistry.suse.com": client.rawBaseURL + "/stg",
		"registry.rancher.com": client.rawBaseURL + "/prime",
		"registry.suse.com":    client.rawBaseURL + "/suse",
		"docker.io":            client.rawBaseURL + "/docker",
	}

	plan, err := buildPlan(context.Background(), client, "v2.14.1-alpha6", "v2.14.0", "", "auto", "", "ha-rancher-rke2/signoff")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantImage := "stgregistry.suse.com/rancher/rancher-webhook:v0.10.1-rc.5"
	if plan.WebhookImage != wantImage {
		t.Fatalf("expected %s, got %s", wantImage, plan.WebhookImage)
	}
	if plan.SigningPolicy != "report-only" {
		t.Fatalf("expected report-only signing policy, got %s", plan.SigningPolicy)
	}
	if plan.Lanes[3].WebhookOverrideImage != wantImage {
		t.Fatalf("expected old webhook lane to use %s, got %s", wantImage, plan.Lanes[3].WebhookOverrideImage)
	}
}

func TestBuildPlanDiscoversReleasedWebhookImageForStableTagWhenNoOverride(t *testing.T) {
	client := fakeGitHubClient(t, map[string]string{
		"/rancher/rancher/v2.13.5-alpha6/build.yaml":          `webhookVersion: 108.0.3+up0.9.3`,
		"/rancher/rancher/v2.13.4/build.yaml":                 `webhookVersion: 108.0.3+up0.9.3`,
		"/suse/v2/rancher/rancher-webhook/manifests/v0.9.3":   "ok",
		"/stg/v2/rancher/rancher-webhook/manifests/v0.9.3":    "ok",
		"/docker/v2/rancher/rancher-webhook/manifests/v0.9.3": "ok",
	})
	client.registryBaseURLs = map[string]string{
		"stgregistry.suse.com": client.rawBaseURL + "/stg",
		"registry.rancher.com": client.rawBaseURL + "/prime",
		"registry.suse.com":    client.rawBaseURL + "/suse",
		"docker.io":            client.rawBaseURL + "/docker",
	}

	plan, err := buildPlan(context.Background(), client, "v2.13.5-alpha6", "v2.13.4", "", "auto", "", "ha-rancher-rke2/signoff")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantImage := "registry.suse.com/rancher/rancher-webhook:v0.9.3"
	if plan.WebhookImage != wantImage {
		t.Fatalf("expected %s, got %s", wantImage, plan.WebhookImage)
	}
	if plan.SigningPolicy != "report-only" {
		t.Fatalf("expected report-only signing policy, got %s", plan.SigningPolicy)
	}
}

func TestBuildPlanFallsBackToDockerHubWhenSUSERegistriesAreMissing(t *testing.T) {
	client := fakeGitHubClient(t, map[string]string{
		"/rancher/rancher/v2.14.1-alpha6/build.yaml":                `webhookVersion: 109.0.1+up0.10.1-rc.5`,
		"/rancher/rancher/v2.14.0/build.yaml":                       `webhookVersion: 109.0.0+up0.10.0`,
		"/docker/v2/rancher/rancher-webhook/manifests/v0.10.1-rc.5": "ok",
	})
	client.registryBaseURLs = map[string]string{
		"stgregistry.suse.com": client.rawBaseURL + "/stg",
		"registry.rancher.com": client.rawBaseURL + "/prime",
		"registry.suse.com":    client.rawBaseURL + "/suse",
		"docker.io":            client.rawBaseURL + "/docker",
	}

	plan, err := buildPlan(context.Background(), client, "v2.14.1-alpha6", "v2.14.0", "", "auto", "", "ha-rancher-rke2/signoff")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantImage := "docker.io/rancher/rancher-webhook:v0.10.1-rc.5"
	if plan.WebhookImage != wantImage {
		t.Fatalf("expected %s, got %s", wantImage, plan.WebhookImage)
	}
	if plan.SigningPolicy != "report-only" {
		t.Fatalf("expected report-only signing policy, got %s", plan.SigningPolicy)
	}
}

func TestBuildPlanFallsBackToPrimeBeforePublicSUSEAndDockerHub(t *testing.T) {
	client := fakeGitHubClient(t, map[string]string{
		"/rancher/rancher/v2.14.1-alpha6/build.yaml":                `webhookVersion: 109.0.1+up0.10.1-rc.5`,
		"/rancher/rancher/v2.14.0/build.yaml":                       `webhookVersion: 109.0.0+up0.10.0`,
		"/prime/v2/rancher/rancher-webhook/manifests/v0.10.1-rc.5":  "ok",
		"/suse/v2/rancher/rancher-webhook/manifests/v0.10.1-rc.5":   "ok",
		"/docker/v2/rancher/rancher-webhook/manifests/v0.10.1-rc.5": "ok",
	})

	plan, err := buildPlan(context.Background(), client, "v2.14.1-alpha6", "v2.14.0", "", "auto", "", "ha-rancher-rke2/signoff")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantImage := "registry.rancher.com/rancher/rancher-webhook:v0.10.1-rc.5"
	if plan.WebhookImage != wantImage {
		t.Fatalf("expected %s, got %s", wantImage, plan.WebhookImage)
	}
	if plan.SigningPolicy != "report-only" {
		t.Fatalf("expected report-only signing policy, got %s", plan.SigningPolicy)
	}
}

func TestBuildPlanFailsWhenExplicitWebhookImageTagMismatchesBuildYAML(t *testing.T) {
	client := fakeGitHubClient(t, map[string]string{
		"/rancher/rancher/v2.14.1-alpha6/build.yaml":             `webhookVersion: 109.0.1+up0.10.1-rc.5`,
		"/rancher/rancher/v2.14.0/build.yaml":                    `webhookVersion: 109.0.0+up0.10.0`,
		"/stg/v2/rancher/rancher-webhook/manifests/v0.10.0":      "ok",
		"/stg/v2/rancher/rancher-webhook/manifests/v0.10.1-rc.5": "ok",
	})

	_, err := buildPlan(context.Background(), client, "v2.14.1-alpha6", "v2.14.0", "stgregistry.suse.com/rancher/rancher-webhook:v0.10.0", "auto", "", "ha-rancher-rke2/signoff")
	if err == nil {
		t.Fatal("expected explicit mismatched webhook image tag to fail")
	}
	if !strings.Contains(err.Error(), "expected v0.10.1-rc.5") {
		t.Fatalf("expected tag mismatch error, got %v", err)
	}
}

func TestBuildPlanFailsWhenExplicitWebhookImageIsMissing(t *testing.T) {
	client := fakeGitHubClient(t, map[string]string{
		"/rancher/rancher/v2.14.1-alpha6/build.yaml": `webhookVersion: 109.0.1+up0.10.1-rc.5`,
		"/rancher/rancher/v2.14.0/build.yaml":        `webhookVersion: 109.0.0+up0.10.0`,
	})

	_, err := buildPlan(context.Background(), client, "v2.14.1-alpha6", "v2.14.0", "stgregistry.suse.com/rancher/rancher-webhook:v0.10.1-rc.5", "auto", "", "ha-rancher-rke2/signoff")
	if err == nil {
		t.Fatal("expected explicit missing webhook image to fail")
	}
	if !strings.Contains(err.Error(), "was not found") {
		t.Fatalf("expected missing image error, got %v", err)
	}
}

func TestRegistryImageTagExistsHandlesBearerChallenge(t *testing.T) {
	var serverURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/auth":
			_, _ = w.Write([]byte(`{"token":"test-token"}`))
		case r.URL.Path == "/v2/rancher/rancher-webhook/manifests/v0.10.1-rc.5":
			if r.Header.Get("Authorization") != "Bearer test-token" {
				w.Header().Set("WWW-Authenticate", `Bearer realm="`+serverURL+`/auth",service="registry",scope="repository:rancher/rancher-webhook:pull"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte("ok"))
		default:
			http.NotFound(w, r)
		}
	}))
	serverURL = server.URL
	t.Cleanup(server.Close)

	client := githubClient{
		httpClient: server.Client(),
		registryBaseURLs: map[string]string{
			"stgregistry.suse.com": server.URL,
		},
	}
	found, err := client.registryImageTagExists(context.Background(), "stgregistry.suse.com", "rancher/rancher-webhook", "v0.10.1-rc.5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Fatal("expected tag to exist")
	}
}

func TestBuildPlanSkipsOldWebhookLaneWhenWebhookUnchanged(t *testing.T) {
	client := fakeGitHubClient(t, map[string]string{
		"/rancher/rancher/v2.14.1-alpha6/build.yaml":                `webhookVersion: 109.0.1+up0.10.1-rc.5`,
		"/rancher/rancher/v2.14.0/build.yaml":                       `webhookVersion: 109.0.1+up0.10.1-rc.5`,
		"/docker/v2/rancher/rancher-webhook/manifests/v0.10.1-rc.5": "ok",
	})

	plan, err := buildPlan(context.Background(), client, "v2.14.1-alpha6", "v2.14.0", "", "auto", "", "ha-rancher-rke2/signoff")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if plan.WebhookChanged {
		t.Fatal("expected webhook to be marked unchanged")
	}
	if plan.SigningPolicy != "report-only" {
		t.Fatalf("expected Docker Hub default to be report-only, got %s", plan.SigningPolicy)
	}
	if len(plan.Lanes) != 3 {
		t.Fatalf("expected 3 lanes, got %d", len(plan.Lanes))
	}
	if plan.Lanes[2].Name != laneLocalSuites {
		t.Fatalf("expected local suites lane, got %s", plan.Lanes[2].Name)
	}
	if len(plan.SkippedLanes) != 1 || plan.SkippedLanes[0].Name != laneOldWebhook {
		t.Fatalf("expected skipped old webhook lane, got %#v", plan.SkippedLanes)
	}
	if plan.Lanes[0].TerraformStateKey != "" {
		t.Fatalf("expected no state key without run id, got %s", plan.Lanes[0].TerraformStateKey)
	}
	if plan.Lanes[0].AWSPrefix != "local-fa" {
		t.Fatalf("unexpected local AWS prefix: %s", plan.Lanes[0].AWSPrefix)
	}
}

func TestBuildTerraformStateKey(t *testing.T) {
	got := buildTerraformStateKey("root/", "v2.14", "v2.14.1-alpha6", "123", laneFreshAlpha)
	want := "root/v2.14/v2.14.1-alpha6/123/fresh-alpha/terraform.tfstate"
	if got != want {
		t.Fatalf("expected %s, got %s", want, got)
	}
}

func TestLatestAlphasPerLineReturnsNewestRecentAlphaPerLine(t *testing.T) {
	targets := latestAlphasPerLineFromReleases([]release{
		{TagName: "v2.14.1-alpha7", Prerelease: true, PublishedAt: "2026-04-24T12:00:00Z"},
		{TagName: "v2.13.5-alpha6", Prerelease: true, PublishedAt: "2026-04-24T11:00:00Z"},
		{TagName: "v2.14.1-alpha6", Prerelease: true, PublishedAt: "2026-04-23T12:00:00Z"},
		{TagName: "v2.12.9-alpha6", Prerelease: true, PublishedAt: "2026-04-24T10:00:00Z"},
		{TagName: "v2.15.0-alpha2", Prerelease: true, PublishedAt: "2026-03-01T12:00:00Z"},
		{TagName: "v2.14.0", Prerelease: false, PublishedAt: "2026-04-20T12:00:00Z"},
	}, time.Date(2026, 3, 26, 0, 0, 0, 0, time.UTC))
	want := []string{"v2.14.1-alpha7", "v2.13.5-alpha6", "v2.12.9-alpha6"}
	if strings.Join(targets, ",") != strings.Join(want, ",") {
		t.Fatalf("expected %v, got %v", want, targets)
	}
}

func TestTargetIgnoreListSkipsReleaseLine(t *testing.T) {
	ignoreList := normalizeTargetIgnoreList(targetIgnoreList{
		ReleaseLines: map[string]string{
			"2.15": "known-bad line",
		},
	})

	reason, ignored := ignoreList.reasonFor("v2.15.0-alpha3")
	if !ignored {
		t.Fatal("expected v2.15 alpha to be ignored")
	}
	if reason != "known-bad line" {
		t.Fatalf("unexpected ignore reason: %s", reason)
	}
}

func TestTargetIgnoreListExactVersionOverridesReleaseLine(t *testing.T) {
	ignoreList := normalizeTargetIgnoreList(targetIgnoreList{
		ReleaseLines: map[string]string{
			"v2.15": "known-bad line",
		},
		Versions: map[string]string{
			"2.15.0-alpha3": "specific broken alpha",
		},
	})

	reason, ignored := ignoreList.reasonFor("v2.15.0-alpha3")
	if !ignored {
		t.Fatal("expected v2.15.0-alpha3 to be ignored")
	}
	if reason != "specific broken alpha" {
		t.Fatalf("expected exact version reason, got %s", reason)
	}
}

func TestIgnoredPlanHasNoRunnableLanes(t *testing.T) {
	plan, err := ignoredPlan("v2.15.0-alpha3", "known-bad line", "123456789", "root")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !plan.Ignored {
		t.Fatal("expected plan to be marked ignored")
	}
	if plan.TargetVersion != "v2.15.0-alpha3" || plan.ReleaseLine != "v2.15" {
		t.Fatalf("unexpected target metadata: %#v", plan)
	}
	if len(plan.Lanes) != 0 {
		t.Fatalf("expected no runnable lanes, got %#v", plan.Lanes)
	}
	if len(plan.SkippedLanes) != 4 {
		t.Fatalf("expected skipped lane records for all lane types, got %#v", plan.SkippedLanes)
	}
}

func TestApplyLedgerSkipsSuccessfulLanes(t *testing.T) {
	plan := plan{
		TargetVersion: "v2.14.1-alpha7",
		Lanes: []lane{
			{Name: laneFreshAlpha},
			{Name: laneUpgradeAlpha},
		},
	}
	ledger := signoffLedger{Entries: map[string]map[string]ledgerEntry{
		"v2.14.1-alpha7": {
			laneFreshAlpha: {
				Status:         "success",
				CoveragePolicy: currentCoveragePolicy,
				RunID:          "123",
				CompletedAt:    "2026-04-25T00:00:00Z",
			},
		},
	}}

	got := applyLedgerSkips(plan, ledger)
	if len(got.Lanes) != 1 || got.Lanes[0].Name != laneUpgradeAlpha {
		t.Fatalf("expected only upgrade lane to remain, got %#v", got.Lanes)
	}
	if len(got.SkippedLanes) != 1 || got.SkippedLanes[0].Name != laneFreshAlpha {
		t.Fatalf("expected fresh lane skip, got %#v", got.SkippedLanes)
	}
}

func TestApplyLedgerDoesNotSkipStaleCoveragePolicy(t *testing.T) {
	plan := plan{
		TargetVersion: "v2.14.1-alpha7",
		Lanes: []lane{
			{Name: laneFreshAlpha},
		},
	}
	ledger := signoffLedger{Entries: map[string]map[string]ledgerEntry{
		"v2.14.1-alpha7": {
			laneFreshAlpha: {
				Status:         "success",
				CoveragePolicy: "alpha-webhook-signoff-v1",
				RunID:          "123",
				CompletedAt:    "2026-04-25T00:00:00Z",
			},
		},
	}}

	got := applyLedgerSkips(plan, ledger)
	if len(got.Lanes) != 1 || got.Lanes[0].Name != laneFreshAlpha {
		t.Fatalf("expected stale coverage entry not to skip lane, got %#v", got.Lanes)
	}
	if len(got.SkippedLanes) != 0 {
		t.Fatalf("expected no skipped lanes, got %#v", got.SkippedLanes)
	}
}

func fakeGitHubClient(t *testing.T, responses map[string]string) githubClient {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		for suffix, body := range responses {
			if strings.HasSuffix(path, strings.TrimPrefix(suffix, "/")) {
				if body == "missing" {
					http.NotFound(w, r)
					return
				}
				_, _ = w.Write([]byte(body))
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)

	return githubClient{
		httpClient: server.Client(),
		token:      "",
		rawBaseURL: server.URL,
		apiBaseURL: server.URL,
		registryBaseURLs: map[string]string{
			"stgregistry.suse.com": server.URL + "/stg",
			"registry.rancher.com": server.URL + "/prime",
			"registry.suse.com":    server.URL + "/suse",
			"docker.io":            server.URL + "/docker",
		},
	}
}
