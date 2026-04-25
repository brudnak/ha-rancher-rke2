package test

import (
	"fmt"
	"os"
	"strings"
)

func maskGitHubActionsValue(value string) {
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		return
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	for _, part := range strings.FieldsFunc(value, func(r rune) bool {
		return r == '\r' || r == '\n'
	}) {
		if strings.TrimSpace(part) != "" {
			fmt.Printf("::add-mask::%s\n", part)
		}
	}
}

func maskGitHubActionsURL(value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}

	maskGitHubActionsValue(value)
	host := rancherTestsHost(value)
	if host == "" {
		return
	}
	maskGitHubActionsValue(host)
	maskGitHubActionsValue("https://" + host)
	maskGitHubActionsValue("http://" + host)
}

func rancherTestsHost(rancherURL string) string {
	host := strings.TrimSpace(rancherURL)
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")
	host = strings.TrimRight(host, "/")
	return host
}
