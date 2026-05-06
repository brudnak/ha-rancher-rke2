package test

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

//go:embed system_readiness.json
var systemReadinessConfigJSON []byte

type systemReadinessConfig struct {
	Tools            []systemReadinessToolConfig    `json:"tools"`
	RequiredEnv      []string                       `json:"required_env"`
	OptionalEnvPairs []systemReadinessEnvPairConfig `json:"optional_env_pairs"`
}

type systemReadinessToolConfig struct {
	Name               string   `json:"name"`
	Command            string   `json:"command"`
	Args               []string `json:"args"`
	VersionPattern     string   `json:"version_pattern"`
	JSONVersionKey     string   `json:"json_version_key"`
	MinimumVersion     string   `json:"minimum_version"`
	RecommendedVersion string   `json:"recommended_version"`
}

type systemReadinessEnvPairConfig struct {
	Name           string   `json:"name"`
	Keys           []string `json:"keys"`
	WarnWhenAbsent bool     `json:"warn_when_absent"`
	AbsentDetail   string   `json:"absent_detail"`
}

type systemReadinessState struct {
	Ready   bool                  `json:"ready"`
	Summary string                `json:"summary"`
	Items   []systemReadinessItem `json:"items"`
}

type systemReadinessItem struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	Detail      string `json:"detail"`
	Version     string `json:"version,omitempty"`
	Recommended string `json:"recommended,omitempty"`
	Minimum     string `json:"minimum,omitempty"`
}

func collectSystemReadiness(configPath string) systemReadinessState {
	cfg := loadSystemReadinessConfig()
	items := make([]systemReadinessItem, 0, len(cfg.Tools)+len(cfg.RequiredEnv)+len(cfg.OptionalEnvPairs)+1)

	for _, tool := range cfg.Tools {
		items = append(items, checkSystemReadinessTool(tool))
	}

	items = append(items, checkToolConfigReadiness(configPath))

	loadSecretEnvironmentFromZProfile()
	for _, envVar := range cfg.RequiredEnv {
		items = append(items, checkRequiredEnvReadiness(envVar))
	}

	for _, pair := range cfg.OptionalEnvPairs {
		items = append(items, checkOptionalEnvPairReadiness(pair))
	}

	ready := true
	warnings := 0
	for _, item := range items {
		switch item.Status {
		case "error":
			ready = false
		case "warning":
			warnings++
		}
	}

	summary := "Ready to resolve plan"
	if !ready {
		summary = "Missing required system readiness checks"
	} else if warnings > 0 {
		summary = fmt.Sprintf("Ready with %d warning(s)", warnings)
	}

	return systemReadinessState{
		Ready:   ready,
		Summary: summary,
		Items:   items,
	}
}

func loadSystemReadinessConfig() systemReadinessConfig {
	var cfg systemReadinessConfig
	if err := json.Unmarshal(systemReadinessConfigJSON, &cfg); err != nil {
		panic(fmt.Sprintf("invalid system_readiness.json: %v", err))
	}
	return cfg
}

func checkSystemReadinessTool(tool systemReadinessToolConfig) systemReadinessItem {
	item := systemReadinessItem{
		Name:        tool.Name,
		Recommended: tool.RecommendedVersion,
		Minimum:     tool.MinimumVersion,
	}

	path, err := exec.LookPath(tool.Command)
	if err != nil {
		item.Status = "error"
		item.Detail = fmt.Sprintf("%s is required but was not found in PATH.", tool.Command)
		return item
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	output, err := exec.CommandContext(ctx, path, tool.Args...).CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		item.Status = "error"
		item.Detail = fmt.Sprintf("%s was found but timed out while checking its version.", tool.Command)
		return item
	}
	if err != nil {
		item.Status = "error"
		item.Detail = fmt.Sprintf("%s was found but failed its version check: %s", tool.Command, strings.TrimSpace(string(output)))
		return item
	}

	version := extractToolVersion(tool, string(output))
	item.Version = version
	if version == "" {
		item.Status = "warning"
		item.Detail = fmt.Sprintf("%s is installed, but its version could not be read. Recommended %s.", tool.Command, tool.RecommendedVersion)
		return item
	}

	if tool.MinimumVersion != "" && compareVersionStrings(version, tool.MinimumVersion) < 0 {
		item.Status = "warning"
		item.Detail = fmt.Sprintf("Found %s. Minimum recommended for this repo is %s; you can try anyway, but errors may be version-related.", version, tool.MinimumVersion)
		return item
	}

	if tool.RecommendedVersion != "" && compareVersionStrings(version, tool.RecommendedVersion) != 0 {
		item.Status = "warning"
		item.Detail = fmt.Sprintf("Found %s. Repo baseline is %s; you can try anyway if this version works for your machine.", version, tool.RecommendedVersion)
		return item
	}

	item.Status = "ok"
	item.Detail = fmt.Sprintf("Found %s.", version)
	return item
}

func extractToolVersion(tool systemReadinessToolConfig, output string) string {
	if tool.JSONVersionKey != "" {
		var values map[string]any
		if err := json.Unmarshal([]byte(output), &values); err == nil {
			if value, ok := values[tool.JSONVersionKey].(string); ok {
				return normalizeToolVersion(value)
			}
		}
	}

	if tool.VersionPattern == "" {
		return ""
	}
	re, err := regexp.Compile(tool.VersionPattern)
	if err != nil {
		return ""
	}
	matches := re.FindStringSubmatch(output)
	if len(matches) < 2 {
		return ""
	}
	return normalizeToolVersion(matches[1])
}

func normalizeToolVersion(version string) string {
	return strings.TrimPrefix(strings.TrimSpace(version), "v")
}

func compareVersionStrings(left, right string) int {
	leftParts := versionParts(left)
	rightParts := versionParts(right)

	for i := 0; i < 3; i++ {
		if leftParts[i] < rightParts[i] {
			return -1
		}
		if leftParts[i] > rightParts[i] {
			return 1
		}
	}
	return 0
}

func versionParts(version string) [3]int {
	var parts [3]int
	fields := strings.Split(normalizeToolVersion(version), ".")
	for i := 0; i < len(fields) && i < 3; i++ {
		n, _ := strconv.Atoi(strings.TrimFunc(fields[i], func(r rune) bool {
			return r < '0' || r > '9'
		}))
		parts[i] = n
	}
	return parts
}

func checkToolConfigReadiness(configPath string) systemReadinessItem {
	item := systemReadinessItem{Name: "tool-config.yml"}
	path := strings.TrimSpace(configPath)
	if path == "" {
		path = filepath.Join("..", "tool-config.yml")
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		absPath = path
	}

	if _, err := os.Stat(absPath); err != nil {
		item.Status = "error"
		item.Detail = fmt.Sprintf("tool-config.yml was not found at %s.", absPath)
		return item
	}

	item.Status = "ok"
	item.Detail = fmt.Sprintf("Found %s.", absPath)
	return item
}

func checkRequiredEnvReadiness(envVar string) systemReadinessItem {
	item := systemReadinessItem{Name: envVar}
	if strings.TrimSpace(os.Getenv(envVar)) == "" {
		item.Status = "error"
		item.Detail = fmt.Sprintf("%s must be set. Value was not read or displayed.", envVar)
		return item
	}

	item.Status = "ok"
	item.Detail = "Set. Value is hidden."
	return item
}

func checkOptionalEnvPairReadiness(pair systemReadinessEnvPairConfig) systemReadinessItem {
	item := systemReadinessItem{Name: pair.Name}
	setCount := 0
	for _, key := range pair.Keys {
		if strings.TrimSpace(os.Getenv(key)) != "" {
			setCount++
		}
	}

	switch {
	case setCount == 0:
		if pair.WarnWhenAbsent {
			item.Status = "warning"
			item.Detail = strings.TrimSpace(pair.AbsentDetail)
			if item.Detail == "" {
				item.Detail = fmt.Sprintf("Not set. Optional, but recommended. Checked %s.", strings.Join(pair.Keys, ", "))
			}
			return item
		}
		item.Status = "ok"
		item.Detail = fmt.Sprintf("Not set. Optional for this run. Checked %s.", strings.Join(pair.Keys, ", "))
	case setCount == len(pair.Keys):
		item.Status = "ok"
		item.Detail = fmt.Sprintf("Set. Values are hidden. Checked %s.", strings.Join(pair.Keys, ", "))
	default:
		item.Status = "warning"
		item.Detail = fmt.Sprintf("Partially set. Either set all or none: %s. Values are hidden.", strings.Join(pair.Keys, ", "))
	}
	return item
}
