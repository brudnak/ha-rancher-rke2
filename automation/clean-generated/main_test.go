package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanGeneratedDryRunDoesNotDelete(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "reports", "v2.14.1-alpha6.md"), "bad report")
	mustWriteFile(t, filepath.Join(root, "reports", ".gitkeep"), "")
	mustWriteFile(t, filepath.Join(root, "automation-output", "signoff-report.md"), "generated")
	mustWriteFile(t, filepath.Join(root, "signoff-plan.json"), "{}")

	c := cleaner{root: root, dryRun: true}
	if err := c.clean(parseTargets("all")); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{
		"reports/v2.14.1-alpha6.md",
		"reports/.gitkeep",
		"automation-output/signoff-report.md",
		"signoff-plan.json",
	} {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Fatalf("dry run removed %s: %v", rel, err)
		}
	}
	if got := strings.Join(c.seen, ","); !strings.Contains(got, "reports/v2.14.1-alpha6.md") || !strings.Contains(got, "automation-output") || !strings.Contains(got, "signoff-plan.json") {
		t.Fatalf("unexpected dry-run seen paths: %v", c.seen)
	}
}

func TestCleanGeneratedApplyDeletesSafeTargets(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "reports", "v2.14.1-alpha6.md"), "bad report")
	mustWriteFile(t, filepath.Join(root, "reports", ".gitkeep"), "")
	mustWriteFile(t, filepath.Join(root, "automation-output", "signoff-report.md"), "generated")
	mustWriteFile(t, filepath.Join(root, "signoff-plan.json"), "{}")

	c := cleaner{root: root, dryRun: false}
	if err := c.clean(parseTargets("all")); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{
		"reports/v2.14.1-alpha6.md",
		"automation-output",
		"signoff-plan.json",
	} {
		if _, err := os.Stat(filepath.Join(root, rel)); !os.IsNotExist(err) {
			t.Fatalf("expected %s to be removed, stat err=%v", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "reports", ".gitkeep")); err != nil {
		t.Fatalf("expected .gitkeep to remain: %v", err)
	}
}

func TestCleanGeneratedRejectsUnsupportedTarget(t *testing.T) {
	c := cleaner{root: t.TempDir(), dryRun: true}
	if err := c.clean([]string{"../nope"}); err == nil {
		t.Fatal("expected unsupported target error")
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
