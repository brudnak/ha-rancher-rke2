package test

import (
	"path/filepath"
	"testing"
)

func TestAutomationOutputDirUsesGitHubWorkspace(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("GITHUB_WORKSPACE", workspace)

	if got, want := automationOutputDir(), filepath.Join(workspace, "automation-output"); got != want {
		t.Fatalf("automationOutputDir() = %q, want %q", got, want)
	}
}

func TestAutomationOutputDirFallsBackToPackageDirectory(t *testing.T) {
	t.Setenv("GITHUB_WORKSPACE", "")

	if got, want := automationOutputDir(), "automation-output"; got != want {
		t.Fatalf("automationOutputDir() = %q, want %q", got, want)
	}
}
