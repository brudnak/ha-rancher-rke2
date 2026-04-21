package test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/viper"
)

func validateLocalToolingPreflight(helmCommands []string) error {
	log.Printf("[preflight] Validating local tooling before provisioning...")

	requiredCommands := []string{"kubectl", "helm", "terraform"}
	for _, commandName := range requiredCommands {
		if _, err := exec.LookPath(commandName); err != nil {
			return fmt.Errorf("%s is required locally but was not found in PATH", commandName)
		}
	}

	if err := refreshHelmRepoIndexes(); err != nil {
		return err
	}

	helmRepoOutput, err := exec.Command("helm", "repo", "list").CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to run 'helm repo list': %w", err)
	}

	missingHelmRepos := findMissingHelmRepos(string(helmRepoOutput), helmCommands)
	if len(missingHelmRepos) > 0 {
		return fmt.Errorf("missing required Helm repos locally: %s", strings.Join(missingHelmRepos, ", "))
	}

	log.Printf("[preflight] Local tooling validated successfully")
	return nil
}

func refreshHelmRepoIndexes() error {
	log.Printf("[preflight] Running 'helm repo update'...")
	helmRepoUpdateOutput, err := exec.Command("helm", "repo", "update").CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to run 'helm repo update': %w", err)
	}
	log.Printf("[preflight] Helm repo update completed (%d bytes)", len(strings.TrimSpace(string(helmRepoUpdateOutput))))
	return nil
}

func validateSecretEnvironment() error {
	loadSecretEnvironmentFromZProfile()

	requiredEnvVars := []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"}
	for _, envVar := range requiredEnvVars {
		if strings.TrimSpace(os.Getenv(envVar)) == "" {
			return fmt.Errorf("%s must be set in the environment", envVar)
		}
	}

	dockerhubUsername := strings.TrimSpace(os.Getenv("DOCKERHUB_USERNAME"))
	dockerhubPassword := strings.TrimSpace(os.Getenv("DOCKERHUB_PASSWORD"))
	if (dockerhubUsername == "") != (dockerhubPassword == "") {
		return fmt.Errorf("set both DOCKERHUB_USERNAME and DOCKERHUB_PASSWORD, or leave both unset")
	}

	log.Printf("[preflight] Secret environment validated successfully")
	return nil
}

func loadSecretEnvironmentFromZProfile() {
	desiredVars := []string{
		"AWS_ACCESS_KEY_ID",
		"AWS_SECRET_ACCESS_KEY",
		"DOCKERHUB_USERNAME",
		"DOCKERHUB_PASSWORD",
	}

	missingVars := 0
	for _, envVar := range desiredVars {
		if strings.TrimSpace(os.Getenv(envVar)) == "" {
			missingVars++
		}
	}
	if missingVars == 0 {
		return
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return
	}

	zprofilePath := filepath.Join(homeDir, ".zprofile")
	content, err := os.ReadFile(zprofilePath)
	if err != nil {
		return
	}

	loadedVars := 0
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.HasPrefix(line, "export ") {
			continue
		}

		parts := strings.SplitN(strings.TrimSpace(strings.TrimPrefix(line, "export ")), "=", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		if !slices.Contains(desiredVars, key) {
			continue
		}
		if strings.TrimSpace(os.Getenv(key)) != "" {
			continue
		}

		value = strings.Trim(value, `"'`)
		if value == "" {
			continue
		}

		if os.Setenv(key, value) == nil {
			loadedVars++
		}
	}

	if loadedVars > 0 {
		log.Printf("[preflight] Loaded %d secret environment value(s) from ~/.zprofile", loadedVars)
	}
}

func findMissingHelmRepos(helmRepoListOutput string, helmCommands []string) []string {
	knownRepos := map[string]bool{}
	for _, line := range strings.Split(helmRepoListOutput, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.EqualFold(fields[0], "NAME") {
			continue
		}
		knownRepos[fields[0]] = true
	}

	missingRepos := map[string]bool{}
	for _, helmCommand := range helmCommands {
		fields := strings.Fields(helmCommand)
		for _, field := range fields {
			if !strings.Contains(field, "/") {
				continue
			}
			if strings.HasPrefix(field, "http://") || strings.HasPrefix(field, "https://") {
				continue
			}
			if strings.HasPrefix(field, "--") {
				continue
			}

			repoName := strings.SplitN(field, "/", 2)[0]
			if repoName == "" || repoName == "." {
				continue
			}
			if !knownRepos[repoName] {
				missingRepos[repoName] = true
			}
			break
		}
	}

	var missing []string
	for repoName := range missingRepos {
		missing = append(missing, repoName)
	}
	slices.Sort(missing)
	return missing
}

func getRKE2InstallScriptURL(rke2Version, expectedInstallerSHA256 string) (string, string, error) {
	if rke2Version == "" {
		return "", "", fmt.Errorf("k8s.version must be set")
	}
	if expectedInstallerSHA256 == "" {
		return "", "", fmt.Errorf("rke2.install_script_sha256 must be set")
	}

	installScriptURL := fmt.Sprintf("https://raw.githubusercontent.com/rancher/rke2/%s/install.sh", rke2Version)
	return installScriptURL, expectedInstallerSHA256, nil
}

func validatePinnedRKE2InstallerChecksum(plans []*RancherResolvedPlan) error {
	log.Printf("[preflight] Validating pinned RKE2 installer checksum before provisioning...")

	if len(plans) == 0 {
		installScriptURL, expectedInstallerSHA256, err := getRKE2InstallScriptURL(
			viper.GetString("k8s.version"),
			viper.GetString("rke2.install_script_sha256"),
		)
		if err != nil {
			return err
		}
		if err := validateSinglePinnedRKE2InstallerChecksum(installScriptURL, expectedInstallerSHA256); err != nil {
			return err
		}
		log.Printf("[preflight] RKE2 installer checksum validated successfully")
		return nil
	}

	seen := map[string]bool{}
	for _, plan := range plans {
		if plan == nil {
			continue
		}

		installScriptURL, expectedInstallerSHA256, err := getRKE2InstallScriptURL(plan.RecommendedRKE2Version, plan.InstallerSHA256)
		if err != nil {
			return err
		}

		dedupKey := installScriptURL + "|" + strings.ToLower(expectedInstallerSHA256)
		if seen[dedupKey] {
			continue
		}
		seen[dedupKey] = true

		if err := validateSinglePinnedRKE2InstallerChecksum(installScriptURL, expectedInstallerSHA256); err != nil {
			return err
		}
	}

	log.Printf("[preflight] RKE2 installer checksum validated successfully")
	return nil
}

func validateSinglePinnedRKE2InstallerChecksum(installScriptURL, expectedInstallerSHA256 string) error {
	resp, err := http.Get(installScriptURL)
	if err != nil {
		return fmt.Errorf("failed to download installer from %s: %w", installScriptURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected HTTP status %d downloading %s", resp.StatusCode, installScriptURL)
	}

	hasher := sha256.New()
	if _, err := io.Copy(hasher, resp.Body); err != nil {
		return fmt.Errorf("failed to hash installer from %s: %w", installScriptURL, err)
	}

	actualInstallerSHA256 := hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(actualInstallerSHA256, expectedInstallerSHA256) {
		return fmt.Errorf("installer checksum mismatch for %s: expected %s, got %s", installScriptURL, expectedInstallerSHA256, actualInstallerSHA256)
	}

	return nil
}

func isRKE2InstallerChecksumFailure(stdout, stderr string) bool {
	combinedOutput := stdout + "\n" + stderr
	return strings.Contains(combinedOutput, "SECURITY ERROR: RKE2 installer checksum validation failed")
}

func buildRKE2InstallCommand(nodeType string, rke2Version string, expectedInstallerSHA256 string) (string, error) {
	installScriptURL, expectedInstallerSHA256, err := getRKE2InstallScriptURL(rke2Version, expectedInstallerSHA256)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf(`tmp_script="$(mktemp /tmp/rke2-install.XXXXXX.sh)"
trap 'rm -f "$tmp_script"' EXIT

# Download the exact installer script for the requested RKE2 version.
curl -fsSL -o "$tmp_script" %s

# Refuse to execute the script unless it matches the pinned checksum.
if ! echo %s"  $tmp_script" | sha256sum -c -; then
  echo "############################################################" >&2
  echo "# SECURITY ERROR: RKE2 installer checksum validation failed #" >&2
  echo "# Refusing to run the downloaded installer.                #" >&2
  echo "# Check the resolved RKE2 version and installer checksum.  #" >&2
  echo "############################################################" >&2
  exit 1
fi

sudo INSTALL_RKE2_VERSION=%s INSTALL_RKE2_TYPE=%s sh "$tmp_script"`,
		shellSingleQuote(installScriptURL),
		shellSingleQuote(expectedInstallerSHA256),
		shellSingleQuote(rke2Version),
		shellSingleQuote(nodeType),
	), nil
}
