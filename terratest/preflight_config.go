package test

import (
	"fmt"
	"strings"

	"github.com/spf13/viper"
)

var editableTFVarKeys = []string{
	"aws_region",
	"aws_prefix",
	"aws_vpc",
	"aws_subnet_a",
	"aws_subnet_b",
	"aws_subnet_c",
	"aws_ami",
	"aws_subnet_id",
	"aws_security_group_id",
	"aws_pem_key_name",
	"aws_route53_fqdn",
}

type editablePreflightConfig struct {
	Distro            string            `json:"distro"`
	BootstrapPassword string            `json:"bootstrapPassword"`
	PreloadImages     bool              `json:"preloadImages"`
	TFVars            map[string]string `json:"tfVars"`
}

func currentEditablePreflightConfig() editablePreflightConfig {
	tfVars := make(map[string]string, len(editableTFVarKeys))
	for _, key := range editableTFVarKeys {
		tfVars[key] = strings.TrimSpace(viper.GetString("tf_vars." + key))
	}
	if prefix, err := normalizeAWSPrefix(tfVars["aws_prefix"]); err == nil {
		tfVars["aws_prefix"] = prefix
	}

	distro := strings.ToLower(strings.TrimSpace(viper.GetString("rancher.distro")))
	if distro == "" {
		distro = "auto"
	}

	return editablePreflightConfig{
		Distro:            distro,
		BootstrapPassword: viper.GetString("rancher.bootstrap_password"),
		PreloadImages:     viper.GetBool("rke2.preload_images"),
		TFVars:            tfVars,
	}
}

func normalizePreflightConfigUpdate(update *preflightConfigUpdate) error {
	if update.TFVars == nil && strings.TrimSpace(update.Distro) == "" && strings.TrimSpace(update.BootstrapPassword) == "" {
		return nil
	}

	update.Distro = strings.ToLower(strings.TrimSpace(update.Distro))
	if update.Distro == "" {
		update.Distro = "auto"
	}
	switch update.Distro {
	case "auto", "community", "prime":
	default:
		return fmt.Errorf("rancher.distro must be auto, community, or prime")
	}

	update.BootstrapPassword = strings.TrimSpace(update.BootstrapPassword)
	if update.BootstrapPassword == "" {
		return fmt.Errorf("rancher.bootstrap_password must be set")
	}

	if update.TFVars == nil {
		return nil
	}

	normalizedPrefix, err := normalizeAWSPrefix(update.TFVars["aws_prefix"])
	if err != nil {
		return err
	}
	update.TFVars["aws_prefix"] = normalizedPrefix
	if strings.TrimSpace(update.TFVars["aws_pem_key_name"]) == "" {
		return fmt.Errorf("tf_vars.aws_pem_key_name must be set")
	}
	for _, key := range editableTFVarKeys {
		update.TFVars[key] = strings.TrimSpace(update.TFVars[key])
	}
	return nil
}

func validateAWSPemKeyNameConfig() error {
	if strings.TrimSpace(viper.GetString("tf_vars.aws_pem_key_name")) == "" {
		return fmt.Errorf("tf_vars.aws_pem_key_name must be set")
	}
	return nil
}
