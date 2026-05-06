package test

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"

	"github.com/spf13/viper"
)

const customHostnameConfigKey = "tf_vars.custom_hostname_prefix"

var customHostnameLabelPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

type preflightConfigUpdate struct {
	Versions              []string          `json:"versions"`
	Distro                string            `json:"distro"`
	BootstrapPassword     string            `json:"bootstrapPassword"`
	PreloadImages         bool              `json:"preloadImages"`
	TFVars                map[string]string `json:"tfVars"`
	CustomHostnameEnabled bool              `json:"customHostnameEnabled"`
	CustomHostnameInput   string            `json:"customHostname"`
}

func currentCustomHostnamePrefix() string {
	prefix, err := configuredCustomHostnamePrefix()
	if err != nil {
		return strings.TrimSpace(viper.GetString(customHostnameConfigKey))
	}
	return prefix
}

func configuredCustomHostnamePrefix() (string, error) {
	raw := sanitizeCustomHostnameText(viper.GetString(customHostnameConfigKey))
	if raw == "" {
		return "", nil
	}
	return normalizeCustomHostnamePrefix(raw, viper.GetString("tf_vars.aws_route53_fqdn"))
}

func normalizeCustomHostnameSelection(enabled bool, input string) (string, error) {
	return normalizeCustomHostnameSelectionForDomain(enabled, input, viper.GetString("tf_vars.aws_route53_fqdn"))
}

func normalizeCustomHostnameSelectionForDomain(enabled bool, input, route53FQDN string) (string, error) {
	if !enabled {
		return "", nil
	}

	prefix, err := normalizeCustomHostnamePrefix(input, route53FQDN)
	if err != nil {
		return "", err
	}
	if prefix == "" {
		return "", fmt.Errorf("custom Rancher URL is enabled, so a hostname label is required")
	}
	return prefix, nil
}

func normalizeCustomHostnamePrefix(input, route53FQDN string) (string, error) {
	value := sanitizeCustomHostnameText(input)
	if value == "" {
		return "", nil
	}

	host := value
	if strings.Contains(host, "://") {
		parsed, err := url.Parse(host)
		if err != nil || parsed.Host == "" {
			return "", fmt.Errorf("custom Rancher URL must be a DNS label or URL under %s", route53FQDN)
		}
		host = parsed.Host
	} else {
		host = strings.Split(host, "/")[0]
	}

	if splitHost, _, err := net.SplitHostPort(host); err == nil {
		host = splitHost
	}

	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	domain := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(route53FQDN)), ".")
	if domain != "" {
		suffix := "." + domain
		switch {
		case host == domain:
			return "", fmt.Errorf("custom Rancher URL must include a hostname before %s", domain)
		case strings.HasSuffix(host, suffix):
			host = strings.TrimSuffix(host, suffix)
		}
	}

	if strings.Contains(host, ".") {
		if domain == "" {
			return "", fmt.Errorf("custom Rancher URL must be a single DNS label")
		}
		return "", fmt.Errorf("custom Rancher URL must be a single DNS label or an FQDN ending in %s", domain)
	}
	if !customHostnameLabelPattern.MatchString(host) {
		return "", fmt.Errorf("custom Rancher hostname %q must be 1-63 lowercase letters, numbers, or hyphens, and cannot start or end with a hyphen", host)
	}

	return host, nil
}

func sanitizeCustomHostnameText(input string) string {
	value := strings.TrimSpace(input)
	for {
		if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
			value = strings.TrimSpace(value[1 : len(value)-1])
			continue
		}
		return value
	}
}

func validateCustomHostnameConfig(totalHAs int) error {
	prefix, err := configuredCustomHostnamePrefix()
	if err != nil {
		return err
	}
	if prefix == "" {
		return nil
	}
	if totalHAs != 1 {
		return fmt.Errorf("%s can only be used when total_has is 1; got total_has=%d", customHostnameConfigKey, totalHAs)
	}
	viper.Set(customHostnameConfigKey, prefix)
	return nil
}
