package test

import "testing"

func TestIsExplicitLifecycleRunAllowsIDEExactPatterns(t *testing.T) {
	tests := []string{
		"TestHaSetup",
		"^TestHaSetup$",
		`^\QTestHaSetup\E$`,
		"^TestHaSetup$/^$",
	}

	for _, pattern := range tests {
		if !isExplicitLifecycleRun(pattern, "TestHaSetup") {
			t.Fatalf("expected pattern %q to explicitly allow TestHaSetup", pattern)
		}
	}
}

func TestIsExplicitLifecycleRunRejectsBroadPatterns(t *testing.T) {
	tests := []string{
		"",
		".*",
		"Test",
		"TestHA",
		"TestHA.*",
		"TestHaSetup|TestHACleanup",
	}

	for _, pattern := range tests {
		if isExplicitLifecycleRun(pattern, "TestHaSetup") {
			t.Fatalf("expected pattern %q to be rejected as too broad", pattern)
		}
	}
}
