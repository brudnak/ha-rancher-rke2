package test

import (
	"log"
	"testing"

	"github.com/spf13/viper"
)

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
