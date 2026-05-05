package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	urlPattern              = regexp.MustCompile("https?://[^\\s\"'<>`)\\]]+")
	rancherHostPattern      = regexp.MustCompile(`\b[a-z0-9-]+\.qa\.rancher\.space\b`)
	ghaNamePattern          = regexp.MustCompile(`gha-[a-z0-9-]+`)
	managementIDNamePattern = regexp.MustCompile(`c-m-[a-z0-9]+`)
)

var droppedJSONKeys = map[string]bool{
	"aws_prefix":               true,
	"candidate_image":          true,
	"claim_types":              true,
	"cluster_name":             true,
	"container":                true,
	"databaseid":               true,
	"deployment":               true,
	"html_url":                 true,
	"id":                       true,
	"ignored_active_runner_id": true,
	"kubeconfig_path":          true,
	"linode_image":             true,
	"linode_region":            true,
	"linode_type":              true,
	"machine_config":           true,
	"management_cluster_id":    true,
	"namespace":                true,
	"previous_image":           true,
	"rancher_host":             true,
	"run_id":                   true,
	"secret_name":              true,
	"state_key_root":           true,
	"terraform_state_key":      true,
	"url":                      true,
}

func main() {
	var sourceDir string
	var outputDir string

	flag.StringVar(&sourceDir, "source", ".", "workspace directory containing smoke artifacts")
	flag.StringVar(&outputDir, "output", "public-smoke-artifacts", "directory to write sanitized artifacts")
	flag.Parse()

	if err := prepareArtifacts(sourceDir, outputDir); err != nil {
		fmt.Fprintf(os.Stderr, "sanitize smoke artifacts: %v\n", err)
		os.Exit(1)
	}
}

func prepareArtifacts(sourceDir, outputDir string) error {
	if err := os.RemoveAll(outputDir); err != nil {
		return err
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return err
	}

	copied := []string{}
	skipped := []string{"lane.env", "automation-output/*.env", "automation-output/*.kubeconfig"}

	if copiedPath, err := sanitizeJSONFile(filepath.Join(sourceDir, "signoff-plan.json"), filepath.Join(outputDir, "signoff-plan.json")); err != nil {
		return err
	} else if copiedPath != "" {
		copied = append(copied, copiedPath)
	}

	jsonFiles, err := filepath.Glob(filepath.Join(sourceDir, "automation-output", "*.json"))
	if err != nil {
		return err
	}
	sort.Strings(jsonFiles)
	for _, sourcePath := range jsonFiles {
		destPath := filepath.Join(outputDir, "automation-output", filepath.Base(sourcePath))
		copiedPath, err := sanitizeJSONFile(sourcePath, destPath)
		if err != nil {
			return err
		}
		if copiedPath != "" {
			copied = append(copied, copiedPath)
		}
	}

	if copiedPath, err := sanitizeTextFile(filepath.Join(sourceDir, "automation-output", "signoff-report.md"), filepath.Join(outputDir, "automation-output", "signoff-report.md")); err != nil {
		return err
	} else if copiedPath != "" {
		copied = append(copied, copiedPath)
	}

	xmlFiles, err := filepath.Glob(filepath.Join(sourceDir, "test-results", "*.xml"))
	if err != nil {
		return err
	}
	sort.Strings(xmlFiles)
	for _, sourcePath := range xmlFiles {
		destPath := filepath.Join(outputDir, "test-results", filepath.Base(sourcePath))
		copiedPath, err := sanitizeTextFile(sourcePath, destPath)
		if err != nil {
			return err
		}
		if copiedPath != "" {
			copied = append(copied, copiedPath)
		}
	}

	return writeSummary(filepath.Join(outputDir, "artifact-summary.md"), copied, skipped)
}

func sanitizeJSONFile(sourcePath, destPath string) (string, error) {
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}

	var value interface{}
	if err := json.Unmarshal(data, &value); err != nil {
		return "", fmt.Errorf("parse %s: %w", sourcePath, err)
	}
	value = sanitizeJSON(value)

	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return "", err
	}
	if err := writeFile(destPath, output.Bytes()); err != nil {
		return "", err
	}
	return filepath.ToSlash(destPath), nil
}

func sanitizeJSON(value interface{}) interface{} {
	switch v := value.(type) {
	case map[string]interface{}:
		cleaned := make(map[string]interface{}, len(v))
		for key, item := range v {
			if droppedJSONKeys[strings.ToLower(key)] {
				continue
			}
			cleaned[key] = sanitizeJSON(item)
		}
		return cleaned
	case []interface{}:
		cleaned := make([]interface{}, 0, len(v))
		for _, item := range v {
			cleaned = append(cleaned, sanitizeJSON(item))
		}
		return cleaned
	case string:
		return sanitizeText(v)
	default:
		return v
	}
}

func sanitizeTextFile(sourcePath, destPath string) (string, error) {
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	if err := writeFile(destPath, []byte(sanitizeText(string(data)))); err != nil {
		return "", err
	}
	return filepath.ToSlash(destPath), nil
}

func sanitizeText(value string) string {
	value = urlPattern.ReplaceAllString(value, "<redacted-url>")
	value = rancherHostPattern.ReplaceAllString(value, "<redacted-host>")
	value = ghaNamePattern.ReplaceAllString(value, "<redacted-name>")
	value = managementIDNamePattern.ReplaceAllString(value, "<redacted-management-id>")
	return value
}

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func writeSummary(path string, copied, skipped []string) error {
	root := filepath.Dir(path)
	sort.Strings(copied)
	sort.Strings(skipped)

	var b strings.Builder
	b.WriteString("# Public Smoke Artifact Summary\n\n")
	b.WriteString("This bundle contains sanitized sign-off outputs for public workflow logs and artifacts.\n\n")
	b.WriteString("Removed or redacted metadata includes Terraform state keys, AWS prefixes, run IDs, Rancher hosts, kubeconfig paths, cluster/resource/secret names, Linode placement details, and URL values.\n\n")
	b.WriteString("## Included\n\n")
	for _, copiedPath := range copied {
		displayPath := copiedPath
		if rel, err := filepath.Rel(root, copiedPath); err == nil && !strings.HasPrefix(rel, "..") {
			displayPath = rel
		}
		fmt.Fprintf(&b, "- `%s`\n", filepath.ToSlash(displayPath))
	}
	b.WriteString("\n## Excluded\n\n")
	for _, path := range skipped {
		fmt.Fprintf(&b, "- `%s`\n", path)
	}
	return writeFile(path, []byte(b.String()))
}
