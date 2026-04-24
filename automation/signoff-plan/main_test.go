package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebhookTagFromBuild(t *testing.T) {
	tag, err := webhookTagFromBuild("109.0.1+up0.10.1-rc.5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tag != "v0.10.1-rc.5" {
		t.Fatalf("expected v0.10.1-rc.5, got %s", tag)
	}
}

func TestParseWebhookBuild(t *testing.T) {
	build, err := parseWebhookBuild(`
defaultShellVersion: rancher/shell:v0.7.0-rc.6
webhookVersion: "109.0.1+up0.10.1-rc.5"
`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if build != "109.0.1+up0.10.1-rc.5" {
		t.Fatalf("unexpected build: %s", build)
	}
}

func TestResolveSigningPolicy(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		registry string
		want     string
	}{
		{name: "suse auto", input: "auto", registry: "registry.suse.com", want: "required"},
		{name: "staging auto", input: "auto", registry: "stgregistry.suse.com", want: "required"},
		{name: "community auto", input: "auto", registry: "docker.io", want: "report-only"},
		{name: "manual skip", input: "skip", registry: "stgregistry.suse.com", want: "skip"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveSigningPolicy(tt.input, tt.registry)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("expected %s, got %s", tt.want, got)
			}
		})
	}
}

func TestBuildPlanAddsOldWebhookLaneWhenWebhookChanged(t *testing.T) {
	client := fakeGitHubClient(t, map[string]string{
		"/rancher/rancher/v2.14.1-alpha6/build.yaml": `webhookVersion: 109.0.1+up0.10.1-rc.5`,
		"/rancher/rancher/v2.14.0/build.yaml":        `webhookVersion: 109.0.0+up0.10.0`,
	})

	plan, err := buildPlan(context.Background(), client, "v2.14.1-alpha6", "v2.14.0", "stgregistry.suse.com/rancher/rancher-webhook:v0.10.1-rc.5", "auto", "123456789", "ha-rancher-rke2/signoff")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !plan.WebhookChanged {
		t.Fatal("expected webhook to be marked changed")
	}
	if plan.SigningPolicy != "required" {
		t.Fatalf("expected required signing policy, got %s", plan.SigningPolicy)
	}
	if len(plan.Lanes) != 4 {
		t.Fatalf("expected 4 lanes, got %d", len(plan.Lanes))
	}
	if plan.Lanes[2].Name != laneLocalSuites {
		t.Fatalf("expected local suites lane, got %s", plan.Lanes[2].Name)
	}
	if plan.Lanes[2].ProvisionDownstream {
		t.Fatal("expected local suites lane to skip downstream provisioning")
	}
	if plan.Lanes[3].Name != laneOldWebhook {
		t.Fatalf("expected old webhook lane, got %s", plan.Lanes[3].Name)
	}
	if plan.Lanes[3].WebhookOverrideImage == "" {
		t.Fatal("expected webhook override image")
	}
	if plan.Lanes[3].TerraformStateKey != "ha-rancher-rke2/signoff/v2.14/v2.14.1-alpha6/123456789/previous-with-candidate-webhook/terraform.tfstate" {
		t.Fatalf("unexpected state key: %s", plan.Lanes[3].TerraformStateKey)
	}
	if plan.Lanes[3].AWSPrefix != "gha-23456789-ow" {
		t.Fatalf("unexpected AWS prefix: %s", plan.Lanes[3].AWSPrefix)
	}
}

func TestBuildPlanSkipsOldWebhookLaneWhenWebhookUnchanged(t *testing.T) {
	client := fakeGitHubClient(t, map[string]string{
		"/rancher/rancher/v2.14.1-alpha6/build.yaml": `webhookVersion: 109.0.1+up0.10.1-rc.5`,
		"/rancher/rancher/v2.14.0/build.yaml":        `webhookVersion: 109.0.1+up0.10.1-rc.5`,
	})

	plan, err := buildPlan(context.Background(), client, "v2.14.1-alpha6", "v2.14.0", "", "auto", "", "ha-rancher-rke2/signoff")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if plan.WebhookChanged {
		t.Fatal("expected webhook to be marked unchanged")
	}
	if plan.SigningPolicy != "report-only" {
		t.Fatalf("expected Docker Hub default to be report-only, got %s", plan.SigningPolicy)
	}
	if len(plan.Lanes) != 3 {
		t.Fatalf("expected 3 lanes, got %d", len(plan.Lanes))
	}
	if plan.Lanes[2].Name != laneLocalSuites {
		t.Fatalf("expected local suites lane, got %s", plan.Lanes[2].Name)
	}
	if len(plan.SkippedLanes) != 1 || plan.SkippedLanes[0].Name != laneOldWebhook {
		t.Fatalf("expected skipped old webhook lane, got %#v", plan.SkippedLanes)
	}
	if plan.Lanes[0].TerraformStateKey != "" {
		t.Fatalf("expected no state key without run id, got %s", plan.Lanes[0].TerraformStateKey)
	}
	if plan.Lanes[0].AWSPrefix != "local-fa" {
		t.Fatalf("unexpected local AWS prefix: %s", plan.Lanes[0].AWSPrefix)
	}
}

func TestBuildTerraformStateKey(t *testing.T) {
	got := buildTerraformStateKey("root/", "v2.14", "v2.14.1-alpha6", "123", laneFreshAlpha)
	want := "root/v2.14/v2.14.1-alpha6/123/fresh-alpha/terraform.tfstate"
	if got != want {
		t.Fatalf("expected %s, got %s", want, got)
	}
}

func fakeGitHubClient(t *testing.T, responses map[string]string) githubClient {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		for suffix, body := range responses {
			if strings.HasSuffix(path, strings.TrimPrefix(suffix, "/")) {
				_, _ = w.Write([]byte(body))
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)

	return githubClient{
		httpClient: server.Client(),
		token:      "",
		rawBaseURL: server.URL,
		apiBaseURL: server.URL,
	}
}
