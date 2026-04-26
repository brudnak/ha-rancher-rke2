package test

import (
	"strings"
	"testing"
)

func TestRenderLinodeDownstreamResources(t *testing.T) {
	cfg := downstreamProvisioningConfig{
		ClusterName:  "test-cluster",
		MachineName:  "nc-test-cluster-pool1-abc12",
		SecretName:   "cc-test-cluster",
		Namespace:    "fleet-default",
		Region:       "us-ord",
		InstanceType: "g6-standard-2",
		Image:        "linode/ubuntu22.04",
		K3SVersion:   "v1.33.4+k3s1",
		LinodeToken:  "secret-token",
	}

	secretManifest := renderLinodeCredentialSecretManifest(cfg)
	secretExpected := []string{
		"kind: Secret",
		"linodecredentialConfig-token: \"secret-token\"",
	}
	for _, snippet := range secretExpected {
		if !strings.Contains(secretManifest, snippet) {
			t.Fatalf("expected secret manifest to contain %q:\n%s", snippet, secretManifest)
		}
	}

	payload := linodeMachineConfigPayload(cfg)
	if payload["type"] != "rke-machine-config.cattle.io.linodeconfig" {
		t.Fatalf("unexpected machine config payload type: %#v", payload["type"])
	}
	if payload["image"] != "linode/ubuntu22.04" || payload["instanceType"] != "g6-standard-2" || payload["region"] != "us-ord" {
		t.Fatalf("unexpected machine config payload: %#v", payload)
	}
	metadata, ok := payload["metadata"].(map[string]interface{})
	if !ok {
		t.Fatalf("machine config payload metadata has unexpected shape: %#v", payload["metadata"])
	}
	if metadata["namespace"] != "fleet-default" || metadata["generateName"] != "nc-test-cluster-pool1-" {
		t.Fatalf("unexpected machine config payload metadata: %#v", metadata)
	}
	if _, ok := payload["interfaces"].([]interface{}); !ok {
		t.Fatalf("machine config payload interfaces has unexpected shape: %#v", payload["interfaces"])
	}

	clusterManifest := renderLinodeDownstreamClusterManifest(cfg)
	expected := []string{
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
		"name: \"nc-test-cluster-pool1-abc12\"",
		"controlPlaneConcurrency: \"1\"",
	}

	for _, snippet := range expected {
		if !strings.Contains(clusterManifest, snippet) {
			t.Fatalf("expected cluster manifest to contain %q:\n%s", snippet, clusterManifest)
		}
	}

	if strings.Contains(clusterManifest, "apiVersion: rke-machine-config.cattle.io/v1") {
		t.Fatalf("machineConfigRef contains API version that Rancher UI does not send:\n%s", clusterManifest)
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

func TestSelectLatestK3SReleaseVersion(t *testing.T) {
	releases := []k3sRelease{
		{Version: "v1.35.3+k3s1"},
		{
			Version:                 "v1.33.8+k3s1",
			MinChannelServerVersion: "v2.12.0-alpha1",
			MaxChannelServerVersion: "v2.14.99",
			ServerArgs:              map[string]interface{}{},
			AgentArgs:               map[string]interface{}{},
		},
		{
			Version:                 "v1.34.6+k3s1",
			MinChannelServerVersion: "v2.13.0-alpha1",
			MaxChannelServerVersion: "v2.15.99",
			ServerArgs:              map[string]interface{}{},
			AgentArgs:               map[string]interface{}{},
		},
		{
			Version:                 "v1.35.2+k3s1",
			MinChannelServerVersion: "v2.14.0-alpha1",
			MaxChannelServerVersion: "v2.15.99",
			ServerArgs:              map[string]interface{}{},
			AgentArgs:               map[string]interface{}{},
		},
		{
			Version:                 "v1.36.0+k3s1",
			MinChannelServerVersion: "v2.16.0-alpha1",
			MaxChannelServerVersion: "v2.16.99",
			ServerArgs:              map[string]interface{}{},
			AgentArgs:               map[string]interface{}{},
		},
	}

	got, err := selectLatestK3SReleaseVersion(releases, "v2.15.0-alpha3")
	if err != nil {
		t.Fatal(err)
	}
	if got != "v1.35.2+k3s1" {
		t.Fatalf("selectLatestK3SReleaseVersion() = %q, want %q", got, "v1.35.2+k3s1")
	}

	got, err = selectLatestK3SReleaseVersion(releases, "v2.13.5")
	if err != nil {
		t.Fatal(err)
	}
	if got != "v1.34.6+k3s1" {
		t.Fatalf("selectLatestK3SReleaseVersion() = %q, want %q", got, "v1.34.6+k3s1")
	}
}

func TestSelectLatestK3SReleaseVersionRequiresCompatibleRange(t *testing.T) {
	releases := []k3sRelease{
		{
			Version:                 "v1.35.2+k3s1",
			MinChannelServerVersion: "v2.14.0-alpha1",
			MaxChannelServerVersion: "v2.15.99",
			ServerArgs:              map[string]interface{}{},
			AgentArgs:               map[string]interface{}{},
		},
	}

	if _, err := selectLatestK3SReleaseVersion(releases, "v2.13.5"); err == nil {
		t.Fatal("expected incompatible K3s release to be rejected")
	}
}

func TestKDMMetadataURLForRancherVersion(t *testing.T) {
	got := kdmMetadataURLForRancherVersion("v2.15.0-alpha3")
	want := "https://releases.rancher.com/kontainer-driver-metadata/dev-v2.15/data.json"
	if got != want {
		t.Fatalf("kdmMetadataURLForRancherVersion() = %q, want %q", got, want)
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
