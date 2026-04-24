package test

import "testing"

func TestSelectLocalWebhookDeploymentByImage(t *testing.T) {
	data := []byte(`{
  "items": [
    {
      "metadata": {"name": "not-it"},
      "spec": {"template": {"spec": {"containers": [
        {"name": "app", "image": "example.com/thing:v1"}
      ]}}}
    },
    {
      "metadata": {"name": "webhook-manager"},
      "spec": {"template": {"spec": {"containers": [
        {"name": "manager", "image": "docker.io/rancher/rancher-webhook:v0.10.1"}
      ]}}}
    }
  ]
}`)

	target, err := selectLocalWebhookDeployment(data)
	if err != nil {
		t.Fatal(err)
	}
	if target.DeploymentName != "webhook-manager" {
		t.Fatalf("DeploymentName = %q, want webhook-manager", target.DeploymentName)
	}
	if target.Namespace != "cattle-system" {
		t.Fatalf("Namespace = %q, want cattle-system", target.Namespace)
	}
	if target.ContainerName != "manager" {
		t.Fatalf("ContainerName = %q, want manager", target.ContainerName)
	}
	if target.CurrentImage != "docker.io/rancher/rancher-webhook:v0.10.1" {
		t.Fatalf("CurrentImage = %q", target.CurrentImage)
	}
}

func TestSelectWebhookDeploymentAllNamespaces(t *testing.T) {
	data := []byte(`{
  "items": [
    {
      "metadata": {"name": "rancher-webhook", "namespace": "cattle-system"},
      "spec": {"template": {"spec": {"containers": [
        {"name": "rancher-webhook", "image": "staging.example/rancher-webhook:v1"}
      ]}}}
    }
  ]
}`)

	target, err := selectWebhookDeployment(data, "")
	if err != nil {
		t.Fatal(err)
	}
	if target.Namespace != "cattle-system" {
		t.Fatalf("Namespace = %q, want cattle-system", target.Namespace)
	}
}

func TestSelectLocalWebhookDeploymentErrorsWhenMissing(t *testing.T) {
	_, err := selectLocalWebhookDeployment([]byte(`{"items":[]}`))
	if err == nil {
		t.Fatal("expected missing webhook deployment error")
	}
}
