package test

import "testing"

func TestTerraformBackendConfigFromEnvEmptyUsesLocalState(t *testing.T) {
	t.Setenv("TF_STATE_BUCKET", "")
	t.Setenv("TF_STATE_LOCK_TABLE", "")
	t.Setenv("TF_STATE_REGION", "")
	t.Setenv("TF_STATE_KEY", "")

	backendConfig, err := terraformBackendConfigFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if backendConfig != nil {
		t.Fatalf("expected nil backend config, got %#v", backendConfig)
	}
}

func TestTerraformBackendConfigFromEnvRequiresAllValues(t *testing.T) {
	t.Setenv("TF_STATE_BUCKET", "bucket")
	t.Setenv("TF_STATE_LOCK_TABLE", "")
	t.Setenv("TF_STATE_REGION", "us-east-2")
	t.Setenv("TF_STATE_KEY", "state.tfstate")

	if _, err := terraformBackendConfigFromEnv(); err == nil {
		t.Fatal("expected error for incomplete backend config")
	}
}

func TestTerraformBackendConfigFromEnvBuildsS3Backend(t *testing.T) {
	t.Setenv("TF_STATE_BUCKET", "bucket")
	t.Setenv("TF_STATE_LOCK_TABLE", "locks")
	t.Setenv("TF_STATE_REGION", "us-east-2")
	t.Setenv("TF_STATE_KEY", "ha-rancher-rke2/signoff/v2.14/v2.14.1-alpha6/123/fresh-alpha/terraform.tfstate")

	backendConfig, err := terraformBackendConfigFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := map[string]interface{}{
		"bucket":         "bucket",
		"key":            "ha-rancher-rke2/signoff/v2.14/v2.14.1-alpha6/123/fresh-alpha/terraform.tfstate",
		"region":         "us-east-2",
		"dynamodb_table": "locks",
		"encrypt":        true,
	}

	for key, want := range expected {
		if got := backendConfig[key]; got != want {
			t.Fatalf("backendConfig[%s]: expected %#v, got %#v", key, want, got)
		}
	}
}
