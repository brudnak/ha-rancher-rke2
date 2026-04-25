package test

import (
	"strings"
	"testing"
)

func TestRenderLinodeDownstreamManifests(t *testing.T) {
	cfg := downstreamProvisioningConfig{
		ClusterName:  "test-cluster",
		MachineName:  "test-cluster-pool1",
		SecretName:   "cc-test-cluster",
		Namespace:    "fleet-default",
		Region:       "us-ord",
		InstanceType: "g6-standard-2",
		Image:        "linode/ubuntu22.04",
		K3SVersion:   "v1.33.4+k3s1",
		RootPassword: "Rancher-test-aA1!",
		Tags:         "ha-rancher-rke2,test-cluster",
		LinodeToken:  "secret-token",
	}

	manifest := renderLinodeDownstreamManifests(cfg)
	expected := []string{
		"kind: Secret",
		"linodecredentialConfig-token: \"secret-token\"",
		"kind: LinodeConfig",
		"instanceType: \"g6-standard-2\"",
		"kind: Cluster",
		"cloudCredentialSecretName: \"cattle-global-data:cc-test-cluster\"",
		"kubernetesVersion: \"v1.33.4+k3s1\"",
		"defaultPodSecurityAdmissionConfigurationTemplateName: \"\"",
		"disable-cloud-controller: false",
		"machineSelectorConfig:",
		"protect-kernel-defaults: false",
		"registries:",
		"controlPlaneRole: true",
		"etcdRole: true",
		"workerRole: true",
		"quantity: 1",
		"machineConfigRef:",
		"kind: LinodeConfig",
		"controlPlaneConcurrency: \"1\"",
	}

	for _, snippet := range expected {
		if !strings.Contains(manifest, snippet) {
			t.Fatalf("expected manifest to contain %q:\n%s", snippet, manifest)
		}
	}

	if strings.Contains(manifest, "\ntype: rke-machine-config.cattle.io.linodeconfig\n") {
		t.Fatalf("LinodeConfig manifest contains obsolete type field:\n%s", manifest)
	}

	if strings.Contains(manifest, "\n        apiVersion: rke-machine-config.cattle.io/v1\n") {
		t.Fatalf("machineConfigRef contains API version that Rancher UI does not send:\n%s", manifest)
	}
}

func TestDNSLabel(t *testing.T) {
	got := dnsLabel("HA_Rancher_RKE2/Some Lane!!")
	if got != "ha-rancher-rke2-some-lane" {
		t.Fatalf("dnsLabel() = %q", got)
	}
}

func TestNormalizeK3SVersion(t *testing.T) {
	tests := map[string]string{
		"1.35.3+k3s1":    "v1.35.3+k3s1",
		" v1.34.6+k3s1 ": "v1.34.6+k3s1",
		"":               "",
	}

	for input, expected := range tests {
		if got := normalizeK3SVersion(input); got != expected {
			t.Fatalf("normalizeK3SVersion(%q) = %q, want %q", input, got, expected)
		}
	}
}

func TestShortRunID(t *testing.T) {
	if got := shortRunID("1234567890"); got != "34567890" {
		t.Fatalf("shortRunID() = %q", got)
	}
}

func TestSummarizeProvisioningClusterStatus(t *testing.T) {
	status := provisioningClusterStatus{}
	status.Status.Phase = "Updating"
	status.Status.Ready = false
	status.Status.Conditions = append(status.Status.Conditions, struct {
		Type    string `json:"type"`
		Status  string `json:"status"`
		Reason  string `json:"reason"`
		Message string `json:"message"`
	}{Type: "Ready", Status: "False", Reason: "Waiting", Message: "node pending"})

	summary := summarizeProvisioningClusterStatus(status)
	if !strings.Contains(summary, "phase=Updating ready=false") || !strings.Contains(summary, "Ready=False/Waiting node pending") {
		t.Fatalf("unexpected summary: %s", summary)
	}
}
