package test

import (
	"os"
	"path/filepath"
	"strings"
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

func TestCleanupAutomationOutputRemovesWorkspaceFolder(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("GITHUB_WORKSPACE", workspace)

	outputDir := automationOutputDir()
	if err := os.MkdirAll(filepath.Join(outputDir, "control-panel"), 0o755); err != nil {
		t.Fatalf("failed to create automation output dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outputDir, "control-panel", "stale.yaml"), []byte("stale"), 0o600); err != nil {
		t.Fatalf("failed to write stale kubeconfig: %v", err)
	}

	cleanupAutomationOutput()

	if _, err := os.Stat(outputDir); !os.IsNotExist(err) {
		t.Fatalf("expected automation output dir to be removed, stat err=%v", err)
	}
}

func TestCreateInstallScriptFailsFastAndCreatesNamespaceIdempotently(t *testing.T) {
	tempDir := t.TempDir()
	originalDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get original dir: %v", err)
	}
	if err := os.Chdir(tempDir); err != nil {
		t.Fatalf("failed to chdir to temp dir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(originalDir); err != nil {
			t.Fatalf("failed to restore original dir: %v", err)
		}
	})

	CreateInstallScript("helm install rancher rancher-latest/rancher", "high-availability-1")

	scriptPath := filepath.Join(tempDir, "high-availability-1", "install.sh")
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("failed to read generated install script: %v", err)
	}
	script := string(data)

	for _, want := range []string{
		"set -euo pipefail",
		"kubectl create namespace cattle-system --dry-run=client -o yaml | kubectl apply -f -",
		"helm install rancher rancher-latest/rancher",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("generated install script missing %q:\n%s", want, script)
		}
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
