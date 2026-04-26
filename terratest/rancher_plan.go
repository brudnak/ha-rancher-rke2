package test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"

	goversion "github.com/hashicorp/go-version"
	"github.com/spf13/viper"
	"golang.org/x/net/html"
)

const (
	rancherHelmOperationInstall = "install"
	rancherHelmOperationUpgrade = "upgrade"
)

var rancherRegistryHTTPClient = http.DefaultClient
var rancherRegistryBaseURLs = map[string]string{}

func prepareRancherConfiguration(totalHAs int) ([]*RancherResolvedPlan, error) {
	mode := strings.ToLower(strings.TrimSpace(viper.GetString("rancher.mode")))
	switch mode {
	case "", "manual":
		return prepareManualRKE2Plans(totalHAs)
	case "auto":
		plans, err := resolveAutoRancherPlans(totalHAs)
		if err != nil {
			return nil, err
		}

		var helmCommands []string
		for _, plan := range plans {
			helmCommands = append(helmCommands, plan.HelmCommands...)
		}
		viper.Set("rancher.helm_commands", helmCommands)
		return plans, nil
	default:
		return nil, fmt.Errorf("unsupported rancher.mode %q", mode)
	}
}

func prepareManualRKE2Plans(totalHAs int) ([]*RancherResolvedPlan, error) {
	versions, err := getRequestedRKE2Versions(totalHAs)
	if err != nil {
		return nil, err
	}

	plans := make([]*RancherResolvedPlan, 0, len(versions))
	for _, version := range versions {
		checksum, err := rke2ChecksumForVersion(version)
		if err != nil {
			return nil, err
		}

		plans = append(plans, &RancherResolvedPlan{
			Mode:                   "manual",
			RecommendedRKE2Version: version,
			InstallerSHA256:        checksum,
		})
	}

	return plans, nil
}

func resolveAutoRancherPlans(totalHAs int) ([]*RancherResolvedPlan, error) {
	requestedVersions, err := getRequestedRancherVersions(totalHAs)
	if err != nil {
		return nil, err
	}

	requestedDistro := strings.ToLower(strings.TrimSpace(viper.GetString("rancher.distro")))
	if requestedDistro == "" {
		requestedDistro = "auto"
	}

	bootstrapPassword := viper.GetString("rancher.bootstrap_password")
	if bootstrapPassword == "" {
		return nil, fmt.Errorf("rancher.bootstrap_password must be set when rancher.mode=auto")
	}

	repoAliases := map[string]bool{}
	for _, requestedVersion := range requestedVersions {
		buildType, _, err := classifyRancherVersion(requestedVersion)
		if err != nil {
			return nil, err
		}
		repoCandidates, _, _ := chooseRancherSourceCandidates(requestedDistro, buildType)
		for _, repoAlias := range repoCandidates {
			repoAliases[repoAlias] = true
		}
	}
	repoAliases["rancher-latest"] = true
	repoAliases["rancher-prime"] = true
	if err := ensureRancherHelmRepos(mapKeys(repoAliases), false); err != nil {
		return nil, err
	}
	if err := refreshHelmRepoIndexes(); err != nil {
		return nil, err
	}

	plans := make([]*RancherResolvedPlan, 0, len(requestedVersions))
	for _, requestedVersion := range requestedVersions {
		buildType, minorLine, err := classifyRancherVersion(requestedVersion)
		if err != nil {
			return nil, err
		}
		if requestedDistro == "prime" && buildType != "release" {
			return nil, fmt.Errorf("prime distro requires a released Rancher version like 2.13.4")
		}

		repoCandidates, resolvedDistro, explanation := chooseRancherSourceCandidates(requestedDistro, buildType)
		chartRepoAlias, chartVersion, compatibilityBaseline, err := resolveChartAndBaseline(repoCandidates, requestedVersion, minorLine, buildType)
		if err != nil {
			return nil, err
		}
		if buildType != "release" && chartRepoAlias == "rancher-prime" {
			explanation = append(explanation, fmt.Sprintf("Using the latest released Prime chart %s as the baseline chart, then overriding Rancher images to the requested %s build", chartVersion, buildType))
		}

		rancherImage, rancherImageTag, agentImage, imageExplanation := resolveImageSettings(requestedVersion, buildType, resolvedDistro)
		if buildType != "release" && chartVersion == requestedVersion && chartRepoAlias == "rancher-prime" {
			rancherImage = ""
			rancherImageTag = ""
			agentImage = ""
			explanation = append(explanation, fmt.Sprintf("Using exact chart match %s/rancher@%s, so no Rancher image overrides are needed", chartRepoAlias, chartVersion))
		}
		if buildType != "release" && chartVersion == requestedVersion && isExactCommunityPrereleaseChart(chartRepoAlias) {
			if err := validateResolvedRancherImages(rancherImage, rancherImageTag, agentImage); err != nil {
				rancherImage = ""
				agentImage = ""
				imageExplanation = []string{fmt.Sprintf("Staging Rancher image override was unavailable for %s, using exact community chart/image defaults", requestedVersion)}
				explanation = append(explanation, fmt.Sprintf("Using exact chart match %s/rancher@%s with community image defaults", chartRepoAlias, chartVersion))
			} else {
				explanation = append(explanation, fmt.Sprintf("Using exact chart match %s/rancher@%s with explicit staging Rancher image overrides", chartRepoAlias, chartVersion))
			}
		}
		if buildType != "release" && chartVersion == requestedVersion && isExactStagingPrereleaseChart(chartRepoAlias) {
			explanation = append(explanation, fmt.Sprintf("Using exact chart match %s/rancher@%s with explicit staging Rancher image overrides", chartRepoAlias, chartVersion))
		}
		if buildType != "release" && chartRepoAlias == "rancher-latest" {
			rancherImage = ""
			agentImage = ""
			explanation = append(explanation, fmt.Sprintf("Using rancher-latest for this %s build, so only the Rancher image tag is overridden to %s", buildType, rancherImageTag))
		}
		if buildType == "release" && chartRepoAlias == "rancher-prime" {
			rancherImage = "registry.rancher.com/rancher/rancher"
			explanation = append(explanation, fmt.Sprintf("Using Prime chart and Prime Rancher image for released version %s", requestedVersion))
		}
		if err := validateResolvedRancherImages(rancherImage, rancherImageTag, agentImage); err != nil {
			return nil, fmt.Errorf("validate Rancher image settings for %s: %w", requestedVersion, err)
		}
		explanation = append(explanation, imageExplanation...)
		if compatibilityBaseline != requestedVersion {
			explanation = append(explanation, fmt.Sprintf("Using %s as the latest released compatibility baseline for the %s release line", compatibilityBaseline, minorLine))
		}

		supportMatrixURL := buildSupportMatrixURL(compatibilityBaseline)
		highestRKE2Minor, supportExplanation, err := resolveHighestSupportedRKE2Minor(supportMatrixURL)
		if err != nil {
			return nil, err
		}
		explanation = append(explanation, supportExplanation)

		recommendedRKE2Version, err := resolveLatestRKE2Patch(highestRKE2Minor)
		if err != nil {
			return nil, err
		}
		explanation = append(explanation, fmt.Sprintf("Selected %s as the latest available RKE2 patch in the supported v1.%d line", recommendedRKE2Version, highestRKE2Minor))

		installerSHA256, err := resolveInstallerSHA256(recommendedRKE2Version)
		if err != nil {
			return nil, err
		}

		helmCommands := buildAutoHelmCommands(1, rancherHelmOperationInstall, chartRepoAlias, chartVersion, bootstrapPassword, rancherImage, rancherImageTag, agentImage)

		plans = append(plans, &RancherResolvedPlan{
			Mode:                   "auto",
			RequestedVersion:       requestedVersion,
			RequestedDistro:        requestedDistro,
			BuildType:              buildType,
			ResolvedDistro:         resolvedDistro,
			ChartRepoAlias:         chartRepoAlias,
			ChartVersion:           chartVersion,
			RancherImage:           rancherImage,
			RancherImageTag:        rancherImageTag,
			AgentImage:             agentImage,
			CompatibilityBaseline:  compatibilityBaseline,
			SupportMatrixURL:       supportMatrixURL,
			RecommendedRKE2Version: recommendedRKE2Version,
			InstallerSHA256:        installerSHA256,
			HelmCommands:           helmCommands,
			Explanation:            explanation,
		})
	}

	return plans, nil
}

func mapKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func getRequestedRancherVersions(totalHAs int) ([]string, error) {
	requestedVersions := viper.GetStringSlice("rancher.versions")
	if len(requestedVersions) > 0 {
		if len(requestedVersions) != totalHAs {
			return nil, fmt.Errorf("rancher.versions has %d entries but total_has is %d; please provide exactly one Rancher version per HA", len(requestedVersions), totalHAs)
		}

		normalized := make([]string, 0, len(requestedVersions))
		for i, version := range requestedVersions {
			normalizedVersion := normalizeVersionInput(version)
			if normalizedVersion == "" {
				return nil, fmt.Errorf("rancher.versions[%d] must not be empty", i)
			}
			normalized = append(normalized, normalizedVersion)
		}
		return normalized, nil
	}

	requestedVersion := normalizeVersionInput(viper.GetString("rancher.version"))
	if requestedVersion == "" {
		return nil, fmt.Errorf("set rancher.version for a single HA or rancher.versions with %d entries for auto mode", totalHAs)
	}
	if totalHAs > 1 {
		return nil, fmt.Errorf("total_has is %d, so rancher.versions must contain %d versions", totalHAs, totalHAs)
	}

	return []string{requestedVersion}, nil
}

func getRequestedRKE2Versions(totalHAs int) ([]string, error) {
	requestedVersions := viper.GetStringSlice("k8s.versions")
	if len(requestedVersions) > 0 {
		if len(requestedVersions) != totalHAs {
			return nil, fmt.Errorf("k8s.versions has %d entries but total_has is %d; please provide exactly one RKE2 version per HA", len(requestedVersions), totalHAs)
		}

		normalized := make([]string, 0, len(requestedVersions))
		for i, version := range requestedVersions {
			normalizedVersion := strings.TrimSpace(version)
			if normalizedVersion == "" {
				return nil, fmt.Errorf("k8s.versions[%d] must not be empty", i)
			}
			normalized = append(normalized, normalizedVersion)
		}
		return normalized, nil
	}

	requestedVersion := strings.TrimSpace(viper.GetString("k8s.version"))
	if requestedVersion == "" {
		return nil, fmt.Errorf("set k8s.version for a single HA or k8s.versions with %d entries", totalHAs)
	}
	if totalHAs > 1 {
		return nil, fmt.Errorf("total_has is %d, so k8s.versions must contain %d versions in manual mode", totalHAs, totalHAs)
	}

	return []string{requestedVersion}, nil
}

func rke2ChecksumForVersion(version string) (string, error) {
	version = strings.TrimSpace(version)
	if version == "" {
		return "", fmt.Errorf("RKE2 version must not be empty")
	}

	checksums := viper.GetStringMapString("rke2.install_script_sha256s")
	if checksum := strings.TrimSpace(checksums[version]); checksum != "" {
		return checksum, nil
	}

	if strings.TrimSpace(viper.GetString("k8s.version")) == version {
		if checksum := strings.TrimSpace(viper.GetString("rke2.install_script_sha256")); checksum != "" {
			return checksum, nil
		}
	}

	return "", fmt.Errorf("rke2.install_script_sha256s.%s must be set", version)
}

func normalizeVersionInput(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "v")
	return value
}

func classifyRancherVersion(version string) (buildType string, minorLine string, err error) {
	headPattern := regexp.MustCompile(`^\d+\.\d+-head$`)
	alphaPattern := regexp.MustCompile(`^\d+\.\d+\.\d+-alpha\d+$`)
	rcPattern := regexp.MustCompile(`^\d+\.\d+\.\d+-rc\d+$`)
	releasePattern := regexp.MustCompile(`^\d+\.\d+\.\d+$`)

	switch {
	case headPattern.MatchString(version):
		parts := strings.Split(version, "-")
		return "head", parts[0], nil
	case alphaPattern.MatchString(version):
		parts := strings.Split(version, "-")
		return "alpha", strings.Join(strings.Split(parts[0], ".")[:2], "."), nil
	case rcPattern.MatchString(version):
		parts := strings.Split(version, "-")
		return "rc", strings.Join(strings.Split(parts[0], ".")[:2], "."), nil
	case releasePattern.MatchString(version):
		return "release", strings.Join(strings.Split(version, ".")[:2], "."), nil
	default:
		return "", "", fmt.Errorf("unsupported rancher.version format %q", version)
	}
}

func chooseRancherSourceCandidates(requestedDistro, buildType string) ([]string, string, []string) {
	switch requestedDistro {
	case "prime":
		return []string{"rancher-prime"}, "prime", []string{"Prime distro was requested explicitly"}
	case "community":
		switch buildType {
		case "head":
			return []string{"optimus-rancher-latest"}, "community-staging", []string{"Head build requested, using community staging chart sources"}
		case "alpha":
			return []string{"optimus-rancher-alpha", "optimus-rancher-latest", "rancher-alpha", "rancher-latest"}, "community-staging", []string{"Alpha build requested, trying community alpha/staging chart sources first"}
		case "rc":
			return []string{"optimus-rancher-latest", "rancher-latest"}, "community-staging", []string{"RC build requested, trying community staging chart sources first"}
		default:
			return []string{"rancher-latest", "optimus-rancher-latest"}, "community", []string{"Released community build requested"}
		}
	default:
		switch buildType {
		case "head":
			return []string{"rancher-prime", "optimus-rancher-latest", "rancher-latest"}, "community-staging", []string{"Head build requested in auto mode, favoring Prime/staging chart sources before community charts"}
		case "alpha":
			return []string{"rancher-prime", "optimus-rancher-alpha", "optimus-rancher-latest", "rancher-alpha", "rancher-latest"}, "community-staging", []string{"Alpha build requested in auto mode, favoring Prime/staging chart sources before community charts"}
		case "rc":
			return []string{"rancher-prime", "optimus-rancher-latest", "rancher-latest"}, "community-staging", []string{"RC build requested in auto mode, favoring Prime/staging chart sources before community charts"}
		default:
			return []string{"rancher-prime", "optimus-rancher-latest", "rancher-latest"}, "community", []string{"Released build requested in auto mode, favoring Prime/staging chart sources before community charts"}
		}
	}
}

func resolveChartAndBaseline(repoCandidates []string, requestedVersion, minorLine, buildType string) (string, string, string, error) {
	if globalExactMatch, err := findExactRequestedChartAcrossRepos(repoCandidates, requestedVersion); err == nil {
		compatibilityBaseline := requestedVersion
		if buildType != "release" {
			compatibilityBaseline, err = resolveCompatibilityBaseline(minorLine)
			if err != nil {
				compatibilityBaseline = requestedVersion
			}
		}
		log.Printf("[resolver] Global exact Rancher chart match selected for %s: %s/rancher@%s", requestedVersion, globalExactMatch.repoAlias, globalExactMatch.chartVersion)
		return globalExactMatch.repoAlias, globalExactMatch.chartVersion, compatibilityBaseline, nil
	}

	var lastErr error
	var bestMatch *resolvedChartMatch
	for _, repoAlias := range repoCandidates {
		results, err := searchHelmRepoVersions(repoAlias)
		if err != nil {
			log.Printf("[resolver] Repo candidate %s query failed for Rancher %s: %v", repoAlias, requestedVersion, err)
			lastErr = err
			continue
		}
		if len(results) == 0 {
			log.Printf("[resolver] Repo candidate %s returned no Rancher chart versions for %s", repoAlias, requestedVersion)
			continue
		}

		switch buildType {
		case "release":
			hasExactRequested := hasChartVersion(results, requestedVersion)
			log.Printf("[resolver] Repo candidate %s inspection for release %s: exactRequested=%t", repoAlias, requestedVersion, hasExactRequested)
			if hasExactRequested {
				recordResolvedChartMatch(&bestMatch, repoAlias, requestedVersion, requestedVersion, 0)
			}
		default:
			sameMinorRelease, sameMinorReleaseErr := findLatestMinorRelease(results, minorLine)
			compatibilityBaseline, baselineErr := resolveCompatibilityBaseline(minorLine)
			hasExactRequested := hasChartVersion(results, requestedVersion)
			hasCompatibilityBaseline := baselineErr == nil && hasChartVersion(results, compatibilityBaseline)
			if sameMinorReleaseErr != nil {
				log.Printf("[resolver] Repo candidate %s inspection for %s: exactRequested=%t sameMinorRelease=<none> fallbackBaseline=%s fallbackPresent=%t", repoAlias, requestedVersion, hasExactRequested, summarizeBaselineLogValue(compatibilityBaseline, baselineErr), hasCompatibilityBaseline)
			} else {
				log.Printf("[resolver] Repo candidate %s inspection for %s: exactRequested=%t sameMinorRelease=%s fallbackBaseline=%s fallbackPresent=%t", repoAlias, requestedVersion, hasExactRequested, sameMinorRelease, summarizeBaselineLogValue(compatibilityBaseline, baselineErr), hasCompatibilityBaseline)
			}

			if hasChartVersion(results, requestedVersion) {
				if baselineErr != nil {
					compatibilityBaseline = requestedVersion
				}
				recordResolvedChartMatch(&bestMatch, repoAlias, requestedVersion, compatibilityBaseline, 0)
			}

			if sameMinorReleaseErr == nil {
				if baselineErr != nil {
					compatibilityBaseline = sameMinorRelease
				}
				recordResolvedChartMatch(&bestMatch, repoAlias, sameMinorRelease, compatibilityBaseline, 1)
			}

			if baselineErr == nil && hasChartVersion(results, compatibilityBaseline) {
				recordResolvedChartMatch(&bestMatch, repoAlias, compatibilityBaseline, compatibilityBaseline, 2)
			}
			lastErr = sameMinorReleaseErr
		}
	}

	if bestMatch != nil {
		return bestMatch.repoAlias, bestMatch.chartVersion, bestMatch.compatibilityBaseline, nil
	}

	if lastErr != nil {
		return "", "", "", lastErr
	}
	return "", "", "", fmt.Errorf("could not resolve a Rancher chart version for %s from repos %s", requestedVersion, strings.Join(repoCandidates, ", "))
}

func recordResolvedChartMatch(bestMatch **resolvedChartMatch, repoAlias, chartVersion, compatibilityBaseline string, matchRank int) {
	if *bestMatch == nil || matchRank < (*bestMatch).matchRank {
		*bestMatch = &resolvedChartMatch{
			repoAlias:             repoAlias,
			chartVersion:          chartVersion,
			compatibilityBaseline: compatibilityBaseline,
			matchRank:             matchRank,
		}
	}
}

func findExactRequestedChartAcrossRepos(repoCandidates []string, requestedVersion string) (*resolvedChartMatch, error) {
	globalResults, err := searchAllHelmRepoVersions()
	if err != nil {
		return nil, err
	}

	for _, repoAlias := range repoCandidates {
		for _, result := range globalResults {
			if result.Name != fmt.Sprintf("%s/rancher", repoAlias) {
				continue
			}
			if result.Version == requestedVersion || normalizeVersionInput(result.AppVersion) == requestedVersion {
				return &resolvedChartMatch{
					repoAlias:    repoAlias,
					chartVersion: result.Version,
					matchRank:    0,
				}, nil
			}
		}
	}

	return nil, fmt.Errorf("no exact chart match found across repos for Rancher %s", requestedVersion)
}

func summarizeBaselineLogValue(compatibilityBaseline string, err error) string {
	if err != nil {
		return fmt.Sprintf("<unresolved: %v>", err)
	}
	return compatibilityBaseline
}

func resolveCompatibilityBaseline(minorLine string) (string, error) {
	baseline, err := resolveReleasedCompatibilityBaseline(minorLine)
	if err == nil {
		return baseline, nil
	}

	previousMinorLine, previousErr := previousRancherMinorLine(minorLine)
	if previousErr != nil {
		return "", err
	}

	return resolveReleasedCompatibilityBaseline(previousMinorLine)
}

func resolveReleasedCompatibilityBaseline(minorLine string) (string, error) {
	releaseRepos := []string{"rancher-latest", "rancher-prime"}
	var bestVersion *goversion.Version

	for _, repoAlias := range releaseRepos {
		results, err := searchHelmRepoVersions(repoAlias)
		if err != nil {
			continue
		}

		versionString, err := findLatestMinorRelease(results, minorLine)
		if err != nil {
			continue
		}

		parsed, err := goversion.NewVersion(versionString)
		if err != nil {
			continue
		}

		if bestVersion == nil || parsed.GreaterThan(bestVersion) {
			bestVersion = parsed
		}
	}

	if bestVersion == nil {
		return "", fmt.Errorf("no released compatibility baseline found for Rancher %s.x", minorLine)
	}

	return bestVersion.Original(), nil
}

func previousRancherMinorLine(minorLine string) (string, error) {
	parts := strings.Split(minorLine, ".")
	if len(parts) != 2 {
		return "", fmt.Errorf("invalid Rancher minor line %q", minorLine)
	}

	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return "", fmt.Errorf("invalid Rancher major version in %q: %w", minorLine, err)
	}

	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return "", fmt.Errorf("invalid Rancher minor version in %q: %w", minorLine, err)
	}
	if minor == 0 {
		return "", fmt.Errorf("no earlier Rancher minor line exists before %s", minorLine)
	}

	return fmt.Sprintf("%d.%d", major, minor-1), nil
}

func searchHelmRepoVersions(repoAlias string) ([]helmSearchResult, error) {
	chartRef := fmt.Sprintf("%s/rancher", repoAlias)
	output, err := exec.Command("helm", "search", "repo", chartRef, "--devel", "--versions", "-o", "json").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to query helm repo %s: %w", repoAlias, err)
	}

	var results []helmSearchResult
	if err := json.Unmarshal(output, &results); err != nil {
		return nil, fmt.Errorf("failed to parse helm search results for %s: %w", repoAlias, err)
	}
	if len(results) > 0 {
		return results, nil
	}

	globalResults, err := searchAllHelmRepoVersions()
	if err != nil {
		return results, nil
	}

	filteredResults := filterHelmSearchResultsByRepoAlias(globalResults, repoAlias)
	if len(filteredResults) > 0 {
		log.Printf("[resolver] Falling back to global helm search results for repo %s", repoAlias)
		return filteredResults, nil
	}

	return results, nil
}

func searchAllHelmRepoVersions() ([]helmSearchResult, error) {
	output, err := exec.Command("helm", "search", "repo", "--regexp", ".*/rancher$", "--devel", "--versions", "-o", "json").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to query helm repo globally for rancher charts: %w", err)
	}

	var results []helmSearchResult
	if err := json.Unmarshal(output, &results); err != nil {
		return nil, fmt.Errorf("failed to parse global helm search results: %w", err)
	}
	return results, nil
}

func filterHelmSearchResultsByRepoAlias(results []helmSearchResult, repoAlias string) []helmSearchResult {
	chartRefPrefix := repoAlias + "/"
	filteredResults := make([]helmSearchResult, 0)
	for _, result := range results {
		if strings.HasPrefix(result.Name, chartRefPrefix) {
			filteredResults = append(filteredResults, result)
		}
	}
	return filteredResults
}

func hasChartVersion(results []helmSearchResult, version string) bool {
	for _, result := range results {
		if result.Version == version {
			return true
		}
	}
	return false
}

func findLatestMinorRelease(results []helmSearchResult, minorLine string) (string, error) {
	var candidates []*goversion.Version
	for _, result := range results {
		if !strings.HasPrefix(result.Version, minorLine+".") {
			continue
		}
		if strings.Contains(result.Version, "-") {
			continue
		}
		parsed, err := goversion.NewVersion(result.Version)
		if err != nil {
			continue
		}
		candidates = append(candidates, parsed)
	}

	if len(candidates) == 0 {
		return "", fmt.Errorf("no released chart version found for Rancher %s.x", minorLine)
	}

	slices.SortFunc(candidates, func(a, b *goversion.Version) int {
		return b.Compare(a)
	})
	return candidates[0].Original(), nil
}

func resolveImageSettings(requestedVersion, buildType, resolvedDistro string) (string, string, string, []string) {
	switch resolvedDistro {
	case "prime":
		if buildType == "release" {
			return "registry.rancher.com/rancher/rancher", "", "", []string{"Using Rancher Prime registry because distro=prime was requested explicitly"}
		}
		return "registry.rancher.com/rancher/rancher", "v" + requestedVersion, "", []string{"Using Rancher Prime registry because distro=prime was requested explicitly"}
	case "community-staging":
		imageTag := "v" + requestedVersion
		agentImage := fmt.Sprintf("stgregistry.suse.com/rancher/rancher-agent:%s", imageTag)
		return "stgregistry.suse.com/rancher/rancher", imageTag, agentImage, []string{"Using staging Rancher images because the requested version is not a standard released community build"}
	default:
		if buildType == "release" {
			return "", "", "", []string{"Using released community Rancher chart/image defaults"}
		}
		return "", "v" + requestedVersion, "", []string{"Using released community Rancher chart/image settings"}
	}
}

func isExactCommunityPrereleaseChart(chartRepoAlias string) bool {
	return chartRepoAlias == "rancher-alpha" || chartRepoAlias == "rancher-latest"
}

func isExactStagingPrereleaseChart(chartRepoAlias string) bool {
	return strings.HasPrefix(chartRepoAlias, "optimus-") || chartRepoAlias == "rancher-optimus-alpha" || chartRepoAlias == "optimus-s3"
}

func validateResolvedRancherImages(rancherImage, rancherImageTag, agentImage string) error {
	var images []string
	if rancherImage != "" && rancherImageTag != "" {
		images = append(images, rancherImage+":"+rancherImageTag)
	}
	if rancherImage == "" && rancherImageTag != "" {
		images = append(images, "docker.io/rancher/rancher:"+rancherImageTag)
	}
	if agentImage != "" {
		images = append(images, agentImage)
	}

	for _, image := range images {
		registry, repository, tag, err := parseRegistryImage(image)
		if err != nil {
			return err
		}
		found, err := registryImageTagExists(registry, repository, tag)
		if err != nil {
			return fmt.Errorf("%s: %w", image, err)
		}
		if !found {
			return fmt.Errorf("%s was not found in registry", image)
		}
	}
	return nil
}

func registryImageTagExists(registry, repository, tag string) (bool, error) {
	manifestURL := fmt.Sprintf("%s/v2/%s/manifests/%s", registryBaseURL(registry), repository, tag)
	req, err := http.NewRequest(http.MethodHead, manifestURL, nil)
	if err != nil {
		return false, err
	}
	setRegistryManifestAcceptHeader(req)

	resp, err := rancherRegistryHTTPClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	case http.StatusUnauthorized:
		token, err := registryBearerToken(resp.Header.Get("WWW-Authenticate"))
		if err != nil {
			return false, err
		}
		return registryImageTagExistsWithToken(manifestURL, token)
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return false, fmt.Errorf("registry manifest lookup failed: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
}

func registryImageTagExistsWithToken(manifestURL, token string) (bool, error) {
	req, err := http.NewRequest(http.MethodHead, manifestURL, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	setRegistryManifestAcceptHeader(req)

	resp, err := rancherRegistryHTTPClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return false, fmt.Errorf("registry authenticated manifest lookup failed: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
}

func registryBearerToken(authenticate string) (string, error) {
	params, err := parseRegistryBearerChallenge(authenticate)
	if err != nil {
		return "", err
	}
	realm := params["realm"]
	if realm == "" {
		return "", fmt.Errorf("registry Bearer challenge missing realm")
	}

	req, err := http.NewRequest(http.MethodGet, realm, nil)
	if err != nil {
		return "", err
	}
	query := req.URL.Query()
	if service := params["service"]; service != "" {
		query.Set("service", service)
	}
	if scope := params["scope"]; scope != "" {
		query.Set("scope", scope)
	}
	req.URL.RawQuery = query.Encode()

	resp, err := rancherRegistryHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("registry token request failed: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var tokenResponse struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResponse); err != nil {
		return "", err
	}
	token := tokenResponse.Token
	if token == "" {
		token = tokenResponse.AccessToken
	}
	if token == "" {
		return "", fmt.Errorf("registry token response did not include a token")
	}
	return token, nil
}

func parseRegistryBearerChallenge(value string) (map[string]string, error) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(strings.ToLower(value), "bearer ") {
		return nil, fmt.Errorf("unsupported registry auth challenge %q", value)
	}
	value = strings.TrimSpace(value[len("Bearer "):])
	params := map[string]string{}
	for _, part := range strings.Split(value, ",") {
		key, rawValue, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		params[strings.ToLower(strings.TrimSpace(key))] = strings.Trim(strings.TrimSpace(rawValue), `"`)
	}
	return params, nil
}

func setRegistryManifestAcceptHeader(req *http.Request) {
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.docker.distribution.manifest.v2+json",
	}, ", "))
}

func parseRegistryImage(image string) (registry, repository, tag string, err error) {
	image = strings.TrimSpace(image)
	tagStart := strings.LastIndex(image, ":")
	if tagStart < 0 || tagStart == len(image)-1 {
		return "", "", "", fmt.Errorf("image must include a tag: %s", image)
	}
	slash := strings.Index(image, "/")
	if slash < 0 || slash > tagStart {
		return "", "", "", fmt.Errorf("image must include a registry and repository: %s", image)
	}
	registry = image[:slash]
	repository = image[slash+1 : tagStart]
	tag = image[tagStart+1:]
	if registry == "" || repository == "" || tag == "" {
		return "", "", "", fmt.Errorf("invalid image reference: %s", image)
	}
	return registry, repository, tag, nil
}

func registryBaseURL(registry string) string {
	if base := rancherRegistryBaseURLs[registry]; base != "" {
		return strings.TrimRight(base, "/")
	}
	if registry == "docker.io" {
		return "https://registry-1.docker.io"
	}
	return "https://" + registry
}

func buildSupportMatrixURL(releasedVersion string) string {
	pathVersion := strings.ReplaceAll(releasedVersion, ".", "-")
	return fmt.Sprintf("https://www.suse.com/suse-rancher/support-matrix/all-supported-versions/rancher-v%s/", pathVersion)
}

func resolveHighestSupportedRKE2Minor(supportMatrixURL string) (int, string, error) {
	body, err := fetchURLBody(supportMatrixURL)
	if err != nil {
		return 0, "", err
	}

	textContent, err := extractTextFromHTML(body)
	if err != nil {
		return 0, "", fmt.Errorf("failed to parse support matrix page %s: %w", supportMatrixURL, err)
	}

	rke2RangePattern := regexp.MustCompile(`RKE2\s+v1\.(\d+)\s+v1\.(\d+)`)
	matches := rke2RangePattern.FindStringSubmatch(textContent)
	if len(matches) != 3 {
		return 0, "", fmt.Errorf("could not find supported RKE2 range in %s", supportMatrixURL)
	}

	highestMinorVersion, err := goversion.NewVersion(matches[2])
	if err != nil {
		return 0, "", fmt.Errorf("failed to parse supported RKE2 minor %q: %w", matches[2], err)
	}

	majorSegments := strings.Split(highestMinorVersion.Original(), ".")
	if len(majorSegments) == 0 {
		return 0, "", fmt.Errorf("unexpected supported RKE2 minor value %q", highestMinorVersion.Original())
	}

	var highestMinor int
	fmt.Sscanf(matches[2], "%d", &highestMinor)
	return highestMinor, fmt.Sprintf("Support matrix certifies RKE2 from v1.%s through v1.%s", matches[1], matches[2]), nil
}

func resolveLatestRKE2Patch(highestMinor int) (string, error) {
	releaseNotesURL := fmt.Sprintf("https://docs.rke2.io/release-notes/v1.%d.X", highestMinor)
	body, err := fetchURLBody(releaseNotesURL)
	if err != nil {
		return "", err
	}

	pattern := regexp.MustCompile(fmt.Sprintf(`v1\.%d\.\d+\+rke2r\d+`, highestMinor))
	match := pattern.FindString(body)
	if match == "" {
		return "", fmt.Errorf("could not find an RKE2 patch release in %s", releaseNotesURL)
	}
	return match, nil
}

func resolveInstallerSHA256(rke2Version string) (string, error) {
	installScriptURL := fmt.Sprintf("https://raw.githubusercontent.com/rancher/rke2/%s/install.sh", rke2Version)
	body, err := fetchURLBody(installScriptURL)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:]), nil
}

func buildAutoHelmCommands(totalHAs int, operation, chartRepoAlias, chartVersion, bootstrapPassword, rancherImage, rancherImageTag, agentImage string) []string {
	command := buildAutoHelmCommand(operation, chartRepoAlias, chartVersion, bootstrapPassword, rancherImage, rancherImageTag, agentImage)
	commands := make([]string, totalHAs)
	for i := 0; i < totalHAs; i++ {
		commands[i] = command
	}
	return commands
}

func buildAutoHelmCommand(operation, chartRepoAlias, chartVersion, bootstrapPassword, rancherImage, rancherImageTag, agentImage string) string {
	operation = strings.ToLower(strings.TrimSpace(operation))
	if operation == "" {
		operation = rancherHelmOperationInstall
	}

	var baseSettings []string
	switch operation {
	case rancherHelmOperationInstall:
		baseSettings = []string{
			"helm install rancher " + chartRepoAlias + "/rancher \\",
		}
	case rancherHelmOperationUpgrade:
		baseSettings = []string{
			"helm upgrade rancher " + chartRepoAlias + "/rancher \\",
			"  --install \\",
		}
	default:
		panic(fmt.Sprintf("unsupported Rancher Helm operation %q", operation))
	}

	baseSettings = append(baseSettings, []string{
		"  --namespace cattle-system \\",
		"  --version " + chartVersion + " \\",
		"  --set hostname=placeholder \\",
		"  --set bootstrapPassword=" + bootstrapPassword + " \\",
		"  --set tls=external \\",
		"  --set global.cattle.psp.enabled=false \\",
		"  --set agentTLSMode=system-store",
	}...)

	if rancherImage != "" {
		baseSettings = append(baseSettings[:len(baseSettings)-1], append([]string{
			"  --set rancherImage=" + rancherImage + " \\",
		}, baseSettings[len(baseSettings)-1:]...)...)
	}
	if rancherImageTag != "" {
		baseSettings = append(baseSettings[:len(baseSettings)-1], append([]string{
			"  --set rancherImageTag=" + rancherImageTag + " \\",
		}, baseSettings[len(baseSettings)-1:]...)...)
	}
	if agentImage != "" {
		baseSettings = append(baseSettings[:len(baseSettings)-1], append([]string{
			"  --set 'extraEnv[0].name=CATTLE_AGENT_IMAGE' \\",
			"  --set 'extraEnv[0].value=" + agentImage + "' \\",
		}, baseSettings[len(baseSettings)-1:]...)...)
	}

	if operation == rancherHelmOperationUpgrade {
		baseSettings = append(baseSettings[:len(baseSettings)-1], append([]string{
			"  --wait \\",
			"  --wait-for-jobs \\",
			"  --timeout 30m \\",
		}, baseSettings[len(baseSettings)-1:]...)...)
	}

	return strings.Join(baseSettings, "\n")
}

func fetchURLBody(url string) (string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return "", fmt.Errorf("failed to fetch %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected HTTP status %d fetching %s", resp.StatusCode, url)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read %s: %w", url, err)
	}
	return string(body), nil
}

func extractTextFromHTML(document string) (string, error) {
	root, err := html.Parse(strings.NewReader(document))
	if err != nil {
		return "", err
	}

	var textParts []string
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.TextNode {
			text := strings.TrimSpace(node.Data)
			if text != "" {
				textParts = append(textParts, text)
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)

	return strings.Join(textParts, " "), nil
}
