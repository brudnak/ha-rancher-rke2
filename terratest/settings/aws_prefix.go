package settings

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/spf13/viper"
)

var awsPrefixPattern = regexp.MustCompile(`^[a-z]{2,3}$`)

func NormalizeAWSPrefix(value string) (string, error) {
	prefix := strings.ToLower(strings.TrimSpace(value))
	if !awsPrefixPattern.MatchString(prefix) {
		return "", fmt.Errorf("tf_vars.aws_prefix must be 2 or 3 letters, usually your initials; got %q", strings.TrimSpace(value))
	}
	return prefix, nil
}

func ValidateAWSPrefixConfig() error {
	prefix, err := NormalizeAWSPrefix(viper.GetString("tf_vars.aws_prefix"))
	if err != nil {
		return err
	}
	viper.Set("tf_vars.aws_prefix", prefix)
	return nil
}
