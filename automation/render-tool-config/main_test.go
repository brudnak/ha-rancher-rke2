package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderToolConfigForFreshLane(t *testing.T) {
	cfg := renderConfig{
		BootstrapPassword: "secret-password",
		RancherDistro:     "auto",
		PreloadImages:     true,
		AutoApprove:       true,
		AWSRegion:         "us-east-2",
		AWSPrefix:         "gha-23456789-fa",
		AWSVPC:            "vpc-123",
		AWSSubnetA:        "subnet-a",
		AWSSubnetB:        "subnet-b",
		AWSSubnetC:        "subnet-c",
		AWSAMI:            "ami-123",
		AWSSubnetID:       "subnet-main",
		AWSSecurityGroup:  "sg-123",
		AWSPemKeyName:     "key-name",
		AWSRoute53FQDN:    "example.com",
	}
	lane := signoffLane{
		Name:           "fresh-alpha",
		InstallRancher: "v2.14.1-alpha6",
		AWSPrefix:      "gha-23456789-fa",
	}

	rendered := renderToolConfig(cfg, lane)
	assertContains(t, rendered, `version: "2.14.1-alpha6"`)
	assertContains(t, rendered, `bootstrap_password: "secret-password"`)
	assertContains(t, rendered, `auto_approve: true`)
	assertContains(t, rendered, `aws_prefix: "gha-23456789-fa"`)
	assertContains(t, rendered, `total_has: 1`)
}

func TestRendererWritesConfigAndEnvOutput(t *testing.T) {
	tempDir := t.TempDir()
	planPath := filepath.Join(tempDir, "plan.json")
	outputPath := filepath.Join(tempDir, "tool-config.yml")
	envPath := filepath.Join(tempDir, "lane.env")

	planJSON := `{
  "webhook_image": "stgregistry.suse.com/rancher/rancher-webhook:v0.10.1-rc.5",
  "lanes": [
    {
      "name": "fresh-alpha",
      "install_rancher": "v2.14.1-alpha6",
      "upgrade_to_rancher": "v2.14.1-alpha7",
      "terraform_state_key": "state/key.tfstate",
      "aws_prefix": "gha-23456789-fa"
    }
  ]
}`
	if err := os.WriteFile(planPath, []byte(planJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := renderConfig{
		PlanPath:          planPath,
		LaneName:          "fresh-alpha",
		OutputPath:        outputPath,
		EnvOutputPath:     envPath,
		BootstrapPassword: "secret-password",
		RancherDistro:     "auto",
		PreloadImages:     true,
		AutoApprove:       true,
		AWSRegion:         "us-east-2",
		AWSVPC:            "vpc-123",
		AWSSubnetA:        "subnet-a",
		AWSSubnetB:        "subnet-b",
		AWSSubnetC:        "subnet-c",
		AWSAMI:            "ami-123",
		AWSSubnetID:       "subnet-main",
		AWSSecurityGroup:  "sg-123",
		AWSPemKeyName:     "key-name",
		AWSRoute53FQDN:    "example.com",
	}

	plan, err := readPlan(cfg.PlanPath)
	if err != nil {
		t.Fatal(err)
	}
	lane, err := findLane(plan, cfg.LaneName)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AWSPrefix == "" {
		cfg.AWSPrefix = lane.AWSPrefix
	}
	if err := cfg.validate(lane); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.OutputPath, []byte(renderToolConfig(cfg, lane)), 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := renderEnvOutput(plan, lane)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.EnvOutputPath, []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}

	output, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, string(output), `aws_prefix: "gha-23456789-fa"`)

	envOutput, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, string(envOutput), "TF_STATE_KEY=state/key.tfstate")
	assertContains(t, string(envOutput), "SIGNOFF_AWS_PREFIX=gha-23456789-fa")
	assertContains(t, string(envOutput), "RANCHER_UPGRADE_VERSION=v2.14.1-alpha7")
	assertContains(t, string(envOutput), "RANCHER_WEBHOOK_IMAGE=stgregistry.suse.com/rancher/rancher-webhook:v0.10.1-rc.5")
}

func TestRenderEnvOutputRejectsNewlineInjection(t *testing.T) {
	_, err := renderEnvOutput(
		signoffPlan{WebhookImage: "stgregistry.suse.com/rancher/rancher-webhook:v0.10.1-rc.5\nRANCHER_ADMIN_TOKEN=oops"},
		signoffLane{
			Name:           "fresh-alpha",
			InstallRancher: "v2.14.1-alpha7",
		},
	)
	if err == nil {
		t.Fatal("expected newline injection to be rejected")
	}
}

func TestRenderEnvOutputPrefersLaneWebhookOverride(t *testing.T) {
	env, err := renderEnvOutput(
		signoffPlan{WebhookImage: "stgregistry.suse.com/rancher/rancher-webhook:v0.10.1-rc.5"},
		signoffLane{
			Name:                 "previous-with-candidate-webhook",
			InstallRancher:       "v2.14.0",
			WebhookOverrideImage: "registry.rancher.com/rancher/rancher-webhook:v0.10.1-rc.5",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, env, "RANCHER_WEBHOOK_IMAGE=registry.rancher.com/rancher/rancher-webhook:v0.10.1-rc.5")
}

func assertContains(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("expected %q to contain %q", haystack, needle)
	}
}
