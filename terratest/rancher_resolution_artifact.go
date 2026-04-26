package test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type rancherResolutionArtifact struct {
	Phase                  string   `json:"phase"`
	HAIndex                int      `json:"ha_index"`
	RequestedVersion       string   `json:"requested_version,omitempty"`
	RequestedDistro        string   `json:"requested_distro,omitempty"`
	BuildType              string   `json:"build_type,omitempty"`
	ResolvedDistro         string   `json:"resolved_distro,omitempty"`
	ChartRepoAlias         string   `json:"chart_repo_alias,omitempty"`
	ChartVersion           string   `json:"chart_version,omitempty"`
	ChartSource            string   `json:"chart_source,omitempty"`
	RancherImage           string   `json:"rancher_image,omitempty"`
	RancherImageTag        string   `json:"rancher_image_tag,omitempty"`
	AgentImage             string   `json:"agent_image,omitempty"`
	CompatibilityBaseline  string   `json:"compatibility_baseline,omitempty"`
	RecommendedRKE2Version string   `json:"recommended_rke2_version,omitempty"`
	ResolutionNotes        []string `json:"resolution_notes,omitempty"`
}

func writeRancherResolutionArtifact(phase string, instanceNum int, plan *RancherResolvedPlan) error {
	if plan == nil {
		return nil
	}
	artifact := rancherResolutionArtifact{
		Phase:                  phase,
		HAIndex:                instanceNum,
		RequestedVersion:       plan.RequestedVersion,
		RequestedDistro:        plan.RequestedDistro,
		BuildType:              plan.BuildType,
		ResolvedDistro:         plan.ResolvedDistro,
		ChartRepoAlias:         plan.ChartRepoAlias,
		ChartVersion:           plan.ChartVersion,
		ChartSource:            rancherChartSource(plan),
		RancherImage:           plan.RancherImage,
		RancherImageTag:        plan.RancherImageTag,
		AgentImage:             plan.AgentImage,
		CompatibilityBaseline:  plan.CompatibilityBaseline,
		RecommendedRKE2Version: plan.RecommendedRKE2Version,
		ResolutionNotes:        plan.Explanation,
	}
	data, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		return err
	}
	path := automationOutputPath(fmt.Sprintf("rancher-resolution-%s-ha-%d.json", phase, instanceNum))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func rancherChartSource(plan *RancherResolvedPlan) string {
	if plan == nil || plan.ChartRepoAlias == "" || plan.ChartVersion == "" {
		return ""
	}
	return fmt.Sprintf("%s/rancher@%s", plan.ChartRepoAlias, plan.ChartVersion)
}
