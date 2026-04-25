package test

import (
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

func TestShouldDropPrereleaseImageOverrides(t *testing.T) {
	if shouldDropPrereleaseImageOverrides("optimus-rancher-alpha") {
		t.Fatal("expected optimus alpha charts to keep explicit staging image overrides")
	}

	if shouldDropPrereleaseImageOverrides("optimus-rancher-latest") {
		t.Fatal("expected optimus latest charts to keep explicit staging image overrides")
	}

	if !shouldDropPrereleaseImageOverrides("rancher-alpha") {
		t.Fatal("expected rancher-alpha charts to rely on embedded prerelease image settings")
	}

	if !shouldDropPrereleaseImageOverrides("rancher-latest") {
		t.Fatal("expected rancher-latest charts to rely on embedded prerelease image settings")
	}
}

func TestBuildAutoHelmCommandsKeepsStagingOverridesForOptimusAlpha(t *testing.T) {
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
		"--set rancherImage=stgregistry.suse.com/rancher/rancher",
		"--set rancherImageTag=v2.14.1-alpha3",
		"--set 'extraEnv[0].name=CATTLE_AGENT_IMAGE'",
		"--set 'extraEnv[0].value=stgregistry.suse.com/rancher/rancher-agent:v2.14.1-alpha3'",
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

func TestBuildAutoHelmCommandUpgradeUsesSameResolvedSettings(t *testing.T) {
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
		"--set rancherImage=stgregistry.suse.com/rancher/rancher",
		"--set rancherImageTag=v2.14.1-alpha6",
		"--set 'extraEnv[0].name=CATTLE_AGENT_IMAGE'",
		"--set 'extraEnv[0].value=stgregistry.suse.com/rancher/rancher-agent:v2.14.1-alpha6'",
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
