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
