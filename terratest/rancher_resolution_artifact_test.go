package test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteRancherResolutionArtifact(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("GITHUB_WORKSPACE", workspace)

	plan := &RancherResolvedPlan{
		RequestedVersion:       "2.14.1-alpha7",
		RequestedDistro:        "auto",
		BuildType:              "alpha",
		ResolvedDistro:         "community-staging",
		ChartRepoAlias:         "optimus-rancher-alpha",
		ChartVersion:           "2.14.1-alpha7",
		RancherImage:           "stgregistry.suse.com/rancher/rancher",
		RancherImageTag:        "v2.14.1-alpha7",
		AgentImage:             "stgregistry.suse.com/rancher/rancher-agent:v2.14.1-alpha7",
		CompatibilityBaseline:  "2.14.0",
		RecommendedRKE2Version: "v1.34.6+rke2r3",
		Explanation:            []string{"Using exact chart match optimus-rancher-alpha/rancher@2.14.1-alpha7"},
	}

	if err := writeRancherResolutionArtifact("install", 1, plan); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(workspace, "automation-output", "rancher-resolution-install-ha-1.json"))
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	got := string(data)
	for _, want := range []string{
		`"phase": "install"`,
		`"chart_repo_alias": "optimus-rancher-alpha"`,
		`"chart_version": "2.14.1-alpha7"`,
		`"chart_source": "optimus-rancher-alpha/rancher@2.14.1-alpha7"`,
		`"resolved_distro": "community-staging"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected artifact to contain %s:\n%s", want, got)
		}
	}
}
