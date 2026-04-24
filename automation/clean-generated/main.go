package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type cleaner struct {
	root   string
	dryRun bool
	seen   []string
}

func main() {
	var root string
	var targets string
	var dryRun bool

	flag.StringVar(&root, "root", ".", "repository root")
	flag.StringVar(&targets, "targets", "reports", "comma-separated targets: reports, automation-output, signoff-plan, all")
	flag.BoolVar(&dryRun, "dry-run", true, "print what would be removed without deleting it")
	flag.Parse()

	c := cleaner{root: root, dryRun: dryRun}
	if err := c.clean(parseTargets(targets)); err != nil {
		fmt.Fprintf(os.Stderr, "clean generated files: %v\n", err)
		os.Exit(1)
	}
	c.printSummary()
}

func parseTargets(value string) []string {
	parts := strings.Split(value, ",")
	targets := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if part == "all" {
			targets = append(targets, "reports", "automation-output", "signoff-plan")
			continue
		}
		targets = append(targets, part)
	}
	return targets
}

func (c *cleaner) clean(targets []string) error {
	if len(targets) == 0 {
		return fmt.Errorf("no targets selected")
	}
	for _, target := range targets {
		switch target {
		case "reports":
			if err := c.cleanDirContents("reports", map[string]bool{".gitkeep": true}); err != nil {
				return err
			}
		case "automation-output":
			if err := c.removePath("automation-output"); err != nil {
				return err
			}
		case "signoff-plan":
			if err := c.removePath("signoff-plan.json"); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported target %q", target)
		}
	}
	sort.Strings(c.seen)
	return nil
}

func (c *cleaner) cleanDirContents(rel string, preserve map[string]bool) error {
	path, err := c.safePath(rel)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if preserve[entry.Name()] {
			continue
		}
		if err := c.removePath(filepath.Join(rel, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func (c *cleaner) removePath(rel string) error {
	path, err := c.safePath(rel)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	c.seen = append(c.seen, filepath.ToSlash(rel))
	if c.dryRun {
		return nil
	}
	return os.RemoveAll(path)
}

func (c *cleaner) safePath(rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("absolute paths are not allowed: %s", rel)
	}
	root, err := filepath.Abs(c.root)
	if err != nil {
		return "", err
	}
	path := filepath.Clean(filepath.Join(root, rel))
	if path != root && !strings.HasPrefix(path, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes repository root: %s", rel)
	}
	return path, nil
}

func (c *cleaner) printSummary() {
	mode := "Removed"
	if c.dryRun {
		mode = "Would remove"
	}
	fmt.Printf("## Generated cleanup\n\n")
	if len(c.seen) == 0 {
		fmt.Printf("No matching generated files found.\n")
		return
	}
	for _, path := range c.seen {
		fmt.Printf("- %s `%s`\n", mode, path)
	}
}
