package test

import (
	"reflect"
	"strings"
	"testing"
)

func TestHelmRepoAliasFromCommand(t *testing.T) {
	command := `helm upgrade rancher optimus-rancher-alpha/rancher \
  --namespace cattle-system \
  --set hostname=rancher.example.com`

	if got := helmRepoAliasFromCommand(command); got != "optimus-rancher-alpha" {
		t.Fatalf("helmRepoAliasFromCommand() = %q, want optimus-rancher-alpha", got)
	}
}

func TestHelmRepoAliasesFromCommandsDeduplicatesAndSorts(t *testing.T) {
	got := helmRepoAliasesFromCommands([]string{
		"helm install rancher rancher-latest/rancher --namespace cattle-system",
		"helm upgrade rancher optimus-rancher-alpha/rancher --namespace cattle-system",
		"helm upgrade rancher rancher-latest/rancher --namespace cattle-system",
	})

	want := []string{"optimus-rancher-alpha", "rancher-latest"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("helmRepoAliasesFromCommands() = %#v, want %#v", got, want)
	}
}

func TestFindMissingHelmReposAfterKnownRepos(t *testing.T) {
	commands := []string{
		"helm install rancher rancher-latest/rancher --namespace cattle-system",
		"helm install other custom-repo/thing --namespace cattle-system",
	}
	output := `NAME             URL
rancher-latest   https://releases.rancher.com/server-charts/latest
`

	missing := findMissingHelmRepos(output, commands)
	if len(missing) != 1 || missing[0] != "custom-repo" {
		t.Fatalf("findMissingHelmRepos() = %#v, want custom-repo", missing)
	}
}

func TestKnownRancherHelmRepoURLs(t *testing.T) {
	required := []string{
		"rancher-latest",
		"rancher-stable",
		"rancher-alpha",
		"rancher-prime",
		"optimus-rancher-latest",
		"optimus-rancher-alpha",
	}

	for _, repoAlias := range required {
		if rancherHelmRepoURLs[repoAlias] == "" {
			t.Fatalf("expected %s to have a known URL", repoAlias)
		}
	}
}

func TestBuildRKE2ImagesDownloadCommandRetriesDownloadsAndValidatesChecksum(t *testing.T) {
	command := buildRKE2ImagesDownloadCommand("v1.34.6+rke2r3")

	for _, want := range []string{
		"curl -fsSL --retry 5 --retry-all-errors --retry-delay 5 --connect-timeout 20 --max-time 600 -o /tmp/rke2-images.linux-amd64.tar.zst",
		"curl -fsSL --retry 5 --retry-all-errors --retry-delay 5 --connect-timeout 20 --max-time 120 -o /tmp/rke2-sha256sum-amd64.txt",
		"grep 'rke2-images.linux-amd64.tar.zst' /tmp/rke2-sha256sum-amd64.txt | sha256sum -c -",
		"SECURITY ERROR: RKE2 images checksum validation failed",
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("expected RKE2 image download command to contain %q:\n%s", want, command)
		}
	}
}
