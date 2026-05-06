package test

import (
	"log"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/viper"
)

func TestControlPanelKubeconfigNames(t *testing.T) {
	if got := localClusterID(2); got != "ha-2-local" {
		t.Fatalf("expected local cluster id, got %q", got)
	}
	if got := downstreamClusterID(1, "fleet-default", "QA Cluster"); got != "ha-1-downstream-fleet-default-qa-cluster" {
		t.Fatalf("expected downstream cluster id, got %q", got)
	}
	if got := safeKubeconfigDownloadName("QA Cluster"); got != "qa-cluster.yaml" {
		t.Fatalf("expected safe kubeconfig download name, got %q", got)
	}
}

func TestPruneStaleDownstreamKubeconfigsRemovesMissingClusters(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("GITHUB_WORKSPACE", workspace)

	cacheDir := filepath.Join(automationOutputDir(), "control-panel")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatalf("failed to create cache dir: %v", err)
	}

	activeID := downstreamClusterID(1, "fleet-default", "active")
	staleID := downstreamClusterID(1, "fleet-default", "stale")
	otherHAID := downstreamClusterID(2, "fleet-default", "stale")
	for _, id := range []string{activeID, staleID, otherHAID} {
		if err := os.WriteFile(filepath.Join(cacheDir, id+".yaml"), []byte(id), 0o600); err != nil {
			t.Fatalf("failed to write cached kubeconfig: %v", err)
		}
	}

	panel := &localControlPanel{
		downstreamKubeconfigCache: map[string]string{
			activeID: filepath.Join(cacheDir, activeID+".yaml"),
			staleID:  filepath.Join(cacheDir, staleID+".yaml"),
		},
	}

	panel.pruneStaleDownstreamKubeconfigs(1, map[string]bool{activeID: true})

	if _, err := os.Stat(filepath.Join(cacheDir, activeID+".yaml")); err != nil {
		t.Fatalf("expected active kubeconfig to remain: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, staleID+".yaml")); !os.IsNotExist(err) {
		t.Fatalf("expected stale kubeconfig to be removed, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, otherHAID+".yaml")); err != nil {
		t.Fatalf("expected other HA kubeconfig to remain: %v", err)
	}
	if _, ok := panel.downstreamKubeconfigCache[staleID]; ok {
		t.Fatal("expected stale cache entry to be removed")
	}
	if _, ok := panel.downstreamKubeconfigCache[activeID]; !ok {
		t.Fatal("expected active cache entry to remain")
	}
}

func runHAControlPanelTest(t *testing.T) {
	setupConfig(t)

	totalHAs := viper.GetInt("total_has")
	if totalHAs < 1 {
		t.Fatal("total_has must be at least 1")
	}

	panel, err := newLocalControlPanel(totalHAs)
	if err != nil {
		t.Fatalf("Failed to start local control panel: %v", err)
	}

	panel.start()
	log.Printf("[control-panel] Local control panel available at %s", panel.baseURL)

	if err := openBrowser(panel.baseURL); err != nil {
		log.Printf("[control-panel] Failed to open browser automatically: %v", err)
	}

	if err := panel.wait(); err != nil {
		t.Fatalf("Local control panel exited with error: %v", err)
	}
}
