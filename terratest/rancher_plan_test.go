package test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPreviousRancherMinorLine(t *testing.T) {
	previousMinorLine, err := previousRancherMinorLine("2.15")
	if err != nil {
		t.Fatalf("expected previous Rancher minor line, got error: %v", err)
	}

	if previousMinorLine != "2.14" {
		t.Fatalf("expected previous Rancher minor line 2.14, got %s", previousMinorLine)
	}
}

func TestFindLatestMinorReleaseIgnoresPrereleases(t *testing.T) {
	results := []helmSearchResult{
		{Version: "2.15.0-alpha3"},
		{Version: "2.14.1-rc1"},
		{Version: "2.14.1"},
		{Version: "2.14.0"},
	}

	version, err := findLatestMinorRelease(results, "2.14")
	if err != nil {
		t.Fatalf("expected released chart version, got error: %v", err)
	}

	if version != "2.14.1" {
		t.Fatalf("expected latest released 2.14.x chart version, got %s", version)
	}
}

func TestFindLatestMinorReleaseErrorsWithoutGA(t *testing.T) {
	results := []helmSearchResult{
		{Version: "2.15.0-alpha3"},
		{Version: "2.15.0-rc1"},
	}

	_, err := findLatestMinorRelease(results, "2.15")
	if err == nil {
		t.Fatal("expected an error when no released chart version exists")
	}
}

func TestPrereleaseChartClassification(t *testing.T) {
	if !isExactStagingPrereleaseChart("optimus-rancher-alpha") {
		t.Fatal("expected optimus alpha charts to be staging prerelease charts")
	}

	if !isExactStagingPrereleaseChart("optimus-rancher-latest") {
		t.Fatal("expected optimus latest charts to be staging prerelease charts")
	}

	if !isExactCommunityPrereleaseChart("rancher-alpha") {
		t.Fatal("expected rancher-alpha charts to be community prerelease charts")
	}

	if !isExactCommunityPrereleaseChart("rancher-latest") {
		t.Fatal("expected rancher-latest charts to be community prerelease charts")
	}

	if isExactCommunityPrereleaseChart("rancher-prime") || isExactStagingPrereleaseChart("rancher-prime") {
		t.Fatal("expected rancher-prime to use embedded Prime chart image settings")
	}
}

func TestChooseRancherSourceCandidatesAutoPrefersPrimeAndStagingBeforeCommunity(t *testing.T) {
	candidates, _, _ := chooseRancherSourceCandidates("auto", "alpha")
	want := []string{"rancher-prime", "optimus-rancher-alpha", "optimus-rancher-latest", "rancher-alpha", "rancher-latest"}
	if strings.Join(candidates, ",") != strings.Join(want, ",") {
		t.Fatalf("expected %v, got %v", want, candidates)
	}
}

func TestChooseRancherSourceCandidatesAutoReleasePrefersPrimeBeforeCommunity(t *testing.T) {
	candidates, _, _ := chooseRancherSourceCandidates("auto", "release")
	want := []string{"rancher-prime", "optimus-rancher-latest", "rancher-latest"}
	if strings.Join(candidates, ",") != strings.Join(want, ",") {
		t.Fatalf("expected %v, got %v", want, candidates)
	}
}

func TestRecordResolvedChartMatchPrefersExactTargetOverFallbackBaseline(t *testing.T) {
	var best *resolvedChartMatch
	recordResolvedChartMatch(&best, "rancher-prime", "2.14.0", "2.14.0", 1)
	recordResolvedChartMatch(&best, "optimus-rancher-alpha", "2.14.1-alpha7", "2.14.0", 0)

	if best == nil {
		t.Fatal("expected a chart match")
	}
	if best.repoAlias != "optimus-rancher-alpha" || best.chartVersion != "2.14.1-alpha7" {
		t.Fatalf("expected exact alpha chart to beat fallback baseline, got %#v", best)
	}
}

func TestRecordResolvedChartMatchKeepsPrimeOnExactTie(t *testing.T) {
	var best *resolvedChartMatch
	recordResolvedChartMatch(&best, "rancher-prime", "2.14.1-alpha7", "2.14.0", 0)
	recordResolvedChartMatch(&best, "rancher-alpha", "2.14.1-alpha7", "2.14.0", 0)

	if best == nil {
		t.Fatal("expected a chart match")
	}
	if best.repoAlias != "rancher-prime" {
		t.Fatalf("expected first exact Prime match to win the tie, got %#v", best)
	}
}

func TestResolveImageSettingsAllowsMixedReleaseAndAlphaSources(t *testing.T) {
	releaseImage, releaseTag, releaseAgent, _ := resolveImageSettings("2.14.0", "release", "community")
	if releaseImage != "" || releaseTag != "" || releaseAgent != "" {
		t.Fatalf("expected community release to use chart defaults, got image=%q tag=%q agent=%q", releaseImage, releaseTag, releaseAgent)
	}

	alphaImage, alphaTag, alphaAgent, _ := resolveImageSettings("2.14.1-alpha7", "alpha", "community-staging")
	if alphaImage != "stgregistry.suse.com/rancher/rancher" || alphaTag != "v2.14.1-alpha7" {
		t.Fatalf("expected staging Rancher image for alpha, got image=%q tag=%q", alphaImage, alphaTag)
	}
	if alphaAgent != "stgregistry.suse.com/rancher/rancher-agent:v2.14.1-alpha7" {
		t.Fatalf("expected staging agent image for alpha, got %q", alphaAgent)
	}
}

func TestValidateResolvedRancherImagesChecksExplicitRancherAndAgentImages(t *testing.T) {
	var serverURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth":
			_, _ = w.Write([]byte(`{"token":"test-token"}`))
		case "/v2/rancher/rancher/manifests/v2.14.1-alpha7",
			"/v2/rancher/rancher-agent/manifests/v2.14.1-alpha7":
			if r.Header.Get("Authorization") != "Bearer test-token" {
				w.Header().Set("WWW-Authenticate", `Bearer realm="`+serverURL+`/auth",service="registry",scope="repository:rancher/rancher:pull"`)
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

	previousClient := rancherRegistryHTTPClient
	previousBases := rancherRegistryBaseURLs
	rancherRegistryHTTPClient = server.Client()
	rancherRegistryBaseURLs = map[string]string{"stgregistry.suse.com": server.URL}
	t.Cleanup(func() {
		rancherRegistryHTTPClient = previousClient
		rancherRegistryBaseURLs = previousBases
	})

	err := validateResolvedRancherImages(
		"stgregistry.suse.com/rancher/rancher",
		"v2.14.1-alpha7",
		"stgregistry.suse.com/rancher/rancher-agent:v2.14.1-alpha7",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBuildAutoHelmCommandsKeepsStagingOverridesForOptimusAlpha(t *testing.T) {
	t.Setenv("RANCHER_WEBHOOK_IMAGE", "registry.suse.com/rancher/rancher-webhook:v0.9.3")

	commands := buildAutoHelmCommands(
		1,
		rancherHelmOperationInstall,
		"optimus-rancher-alpha",
		"2.14.1-alpha3",
		"admin",
		"stgregistry.suse.com/rancher/rancher",
		"v2.14.1-alpha3",
		"stgregistry.suse.com/rancher/rancher-agent:v2.14.1-alpha3",
	)

	command := commands[0]
	expectedSnippets := []string{
		"--set tls=external",
		"--set systemDefaultRegistry=stgregistry.suse.com",
		"--set rancherImage=stgregistry.suse.com/rancher/rancher",
		"--set rancherImageTag=v2.14.1-alpha3",
		"--set 'extraEnv[0].name=CATTLE_AGENT_IMAGE'",
		"--set 'extraEnv[0].value=rancher/rancher-agent:v2.14.1-alpha3'",
		"--set webhook.global.cattle.systemDefaultRegistry=registry.suse.com",
	}

	for _, snippet := range expectedSnippets {
		if !strings.Contains(command, snippet) {
			t.Fatalf("expected helm command to contain %q, got:\n%s", snippet, command)
		}
	}
	if strings.Contains(command, "ingress.tls.source=secret") {
		t.Fatalf("expected external TLS termination, got:\n%s", command)
	}
}

func TestBuildAutoHelmCommandsCanUseCommunityAlphaImageFallback(t *testing.T) {
	commands := buildAutoHelmCommands(
		1,
		rancherHelmOperationInstall,
		"rancher-alpha",
		"2.15.0-alpha3",
		"admin",
		"",
		"v2.15.0-alpha3",
		"",
	)

	command := commands[0]
	expectedSnippets := []string{
		"helm install rancher rancher-alpha/rancher",
		"--set rancherImageTag=v2.15.0-alpha3",
	}

	for _, snippet := range expectedSnippets {
		if !strings.Contains(command, snippet) {
			t.Fatalf("expected helm command to contain %q, got:\n%s", snippet, command)
		}
	}
	if strings.Contains(command, "stgregistry.suse.com") || strings.Contains(command, "CATTLE_AGENT_IMAGE") {
		t.Fatalf("expected community fallback command not to include staging overrides, got:\n%s", command)
	}
}

func TestBuildAutoHelmCommandUpgradeUsesSameResolvedSettings(t *testing.T) {
	t.Setenv("RANCHER_WEBHOOK_IMAGE", "registry.suse.com/rancher/rancher-webhook:v0.9.3")

	command := buildAutoHelmCommand(
		rancherHelmOperationUpgrade,
		"optimus-rancher-alpha",
		"2.14.1-alpha6",
		"admin",
		"stgregistry.suse.com/rancher/rancher",
		"v2.14.1-alpha6",
		"stgregistry.suse.com/rancher/rancher-agent:v2.14.1-alpha6",
	)

	expectedSnippets := []string{
		"helm upgrade rancher optimus-rancher-alpha/rancher",
		"--install",
		"--version 2.14.1-alpha6",
		"--set hostname=placeholder",
		"--set tls=external",
		"--set systemDefaultRegistry=stgregistry.suse.com",
		"--set rancherImage=stgregistry.suse.com/rancher/rancher",
		"--set rancherImageTag=v2.14.1-alpha6",
		"--set 'extraEnv[0].name=CATTLE_AGENT_IMAGE'",
		"--set 'extraEnv[0].value=rancher/rancher-agent:v2.14.1-alpha6'",
		"--set webhook.global.cattle.systemDefaultRegistry=registry.suse.com",
		"--wait",
		"--wait-for-jobs",
		"--timeout 30m",
	}

	for _, snippet := range expectedSnippets {
		if !strings.Contains(command, snippet) {
			t.Fatalf("expected helm command to contain %q, got:\n%s", snippet, command)
		}
	}
	if strings.Contains(command, "ingress.tls.source=secret") {
		t.Fatalf("expected external TLS termination, got:\n%s", command)
	}
}

func TestNormalizeHelmImageSettingsUsesStagingDefaultRegistryForQualifiedAgent(t *testing.T) {
	t.Setenv("RANCHER_WEBHOOK_IMAGE", "registry.suse.com/rancher/rancher-webhook:v0.9.3")

	settings := normalizeHelmImageSettings(
		"stgregistry.suse.com/rancher/rancher",
		"stgregistry.suse.com/rancher/rancher-agent:v2.13.5-alpha6",
	)

	if settings.systemDefaultRegistry != "stgregistry.suse.com" {
		t.Fatalf("expected staging system default registry, got %q", settings.systemDefaultRegistry)
	}
	if settings.rancherImage != "stgregistry.suse.com/rancher/rancher" {
		t.Fatalf("expected qualified Rancher image, got %q", settings.rancherImage)
	}
	if settings.agentImage != "rancher/rancher-agent:v2.13.5-alpha6" {
		t.Fatalf("expected relative agent image, got %q", settings.agentImage)
	}
	if settings.webhookRegistry != "registry.suse.com" {
		t.Fatalf("expected webhook registry override, got %q", settings.webhookRegistry)
	}
}

func TestNormalizeHelmImageSettingsLeavesDefaultRegistryForChartDefaultAgent(t *testing.T) {
	settings := normalizeHelmImageSettings(
		"registry.rancher.com/rancher/rancher",
		"",
	)

	if settings.systemDefaultRegistry != "" {
		t.Fatalf("expected no system default registry override, got %q", settings.systemDefaultRegistry)
	}
	if settings.rancherImage != "registry.rancher.com/rancher/rancher" {
		t.Fatalf("expected qualified Rancher image to be preserved, got %q", settings.rancherImage)
	}
	if settings.agentImage != "" {
		t.Fatalf("expected empty agent image to be preserved, got %q", settings.agentImage)
	}
}

func TestRancherHelmCommandForHAReplacesPlaceholder(t *testing.T) {
	command := buildAutoHelmCommand(
		rancherHelmOperationUpgrade,
		"rancher-alpha",
		"2.14.1-alpha6",
		"admin",
		"",
		"",
		"",
	)

	command = rancherHelmCommandForHA(command, "rancher.example.com")
	if !strings.Contains(command, "--set hostname=rancher.example.com") {
		t.Fatalf("expected hostname replacement, got:\n%s", command)
	}
	if strings.Contains(command, "--set hostname=placeholder") {
		t.Fatalf("expected placeholder to be replaced, got:\n%s", command)
	}
}
