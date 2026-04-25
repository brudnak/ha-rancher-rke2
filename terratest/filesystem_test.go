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

func TestRancherTestsHostRemovesURLScheme(t *testing.T) {
	tests := map[string]string{
		"gha.example.test":          "gha.example.test",
		"https://gha.example.test":  "gha.example.test",
		"https://gha.example.test/": "gha.example.test",
		"http://gha.example.test":   "gha.example.test",
	}

	for input, want := range tests {
		if got := rancherTestsHost(input); got != want {
			t.Fatalf("rancherTestsHost(%q) = %q, want %q", input, got, want)
		}
	}
}
