package test

import (
	"strings"
	"testing"
)

func TestRKE2IngressNginxConfigManifestEnablesForwardedHeaders(t *testing.T) {
	manifest := rke2IngressNginxConfigManifest()
	expectedSnippets := []string{
		"kind: HelmChartConfig",
		"name: rke2-ingress-nginx",
		"namespace: kube-system",
		`use-forwarded-headers: "true"`,
	}

	for _, snippet := range expectedSnippets {
		if !strings.Contains(manifest, snippet) {
			t.Fatalf("expected RKE2 ingress config manifest to contain %q, got:\n%s", snippet, manifest)
		}
	}
}
