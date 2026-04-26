package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type signoffPlan struct {
	TargetVersion string        `json:"target_version"`
	WebhookImage  string        `json:"webhook_image"`
	Lanes         []signoffLane `json:"lanes"`
}

type signoffLane struct {
	Name                 string `json:"name"`
	InstallRancher       string `json:"install_rancher"`
	UpgradeToRancher     string `json:"upgrade_to_rancher"`
	WebhookOverrideImage string `json:"webhook_override_image"`
	TerraformStateKey    string `json:"terraform_state_key"`
	AWSPrefix            string `json:"aws_prefix"`
}

type renderConfig struct {
	PlanPath          string
	LaneName          string
	OutputPath        string
	EnvOutputPath     string
	BootstrapPassword string
	RancherDistro     string
	PreloadImages     bool
	AutoApprove       bool
	AWSRegion         string
	AWSPrefix         string
	AWSVPC            string
	AWSSubnetA        string
	AWSSubnetB        string
	AWSSubnetC        string
	AWSAMI            string
	AWSSubnetID       string
	AWSSecurityGroup  string
	AWSPemKeyName     string
	AWSRoute53FQDN    string
}

func main() {
	cfg := renderConfig{}
	preloadImages := "true"
	autoApprove := "true"

	flag.StringVar(&cfg.PlanPath, "plan", "signoff-plan.json", "sign-off plan JSON path")
	flag.StringVar(&cfg.LaneName, "lane", "fresh-alpha", "lane name to render")
	flag.StringVar(&cfg.OutputPath, "output", "tool-config.yml", "tool-config.yml output path")
	flag.StringVar(&cfg.EnvOutputPath, "env-output", "", "optional GitHub env output path")
	flag.StringVar(&cfg.BootstrapPassword, "bootstrap-password", envOrDefault("RANCHER_BOOTSTRAP_PASSWORD", ""), "Rancher bootstrap password")
	flag.StringVar(&cfg.RancherDistro, "rancher-distro", envOrDefault("RANCHER_DISTRO", "auto"), "Rancher distro")
	flag.StringVar(&preloadImages, "preload-images", envOrDefault("RKE2_PRELOAD_IMAGES", "true"), "whether to preload RKE2 images")
	flag.StringVar(&autoApprove, "auto-approve", envOrDefault("RANCHER_AUTO_APPROVE", "true"), "whether Rancher plan approval is automatic")
	flag.StringVar(&cfg.AWSRegion, "aws-region", envOrDefault("AWS_REGION", "us-east-2"), "AWS region")
	flag.StringVar(&cfg.AWSPrefix, "aws-prefix", envOrDefault("AWS_PREFIX", ""), "AWS resource prefix override")
	flag.StringVar(&cfg.AWSVPC, "aws-vpc", envOrDefault("AWS_VPC", ""), "AWS VPC ID")
	flag.StringVar(&cfg.AWSSubnetA, "aws-subnet-a", envOrDefault("AWS_SUBNET_A", ""), "AWS subnet A ID")
	flag.StringVar(&cfg.AWSSubnetB, "aws-subnet-b", envOrDefault("AWS_SUBNET_B", ""), "AWS subnet B ID")
	flag.StringVar(&cfg.AWSSubnetC, "aws-subnet-c", envOrDefault("AWS_SUBNET_C", ""), "AWS subnet C ID")
	flag.StringVar(&cfg.AWSAMI, "aws-ami", envOrDefault("AWS_AMI", ""), "AWS AMI ID")
	flag.StringVar(&cfg.AWSSubnetID, "aws-subnet-id", envOrDefault("AWS_SUBNET_ID", ""), "AWS subnet ID for instances")
	flag.StringVar(&cfg.AWSSecurityGroup, "aws-security-group-id", envOrDefault("AWS_SECURITY_GROUP_ID", ""), "AWS security group ID")
	flag.StringVar(&cfg.AWSPemKeyName, "aws-pem-key-name", envOrDefault("AWS_PEM_KEY_NAME", ""), "AWS PEM key name")
	flag.StringVar(&cfg.AWSRoute53FQDN, "aws-route53-fqdn", envOrDefault("AWS_ROUTE53_FQDN", ""), "Route53 FQDN")
	flag.Parse()

	var err error
	cfg.PreloadImages, err = strconv.ParseBool(preloadImages)
	if err != nil {
		fatalf("invalid -preload-images value %q: %v", preloadImages, err)
	}
	cfg.AutoApprove, err = strconv.ParseBool(autoApprove)
	if err != nil {
		fatalf("invalid -auto-approve value %q: %v", autoApprove, err)
	}

	plan, err := readPlan(cfg.PlanPath)
	if err != nil {
		fatalf("read plan: %v", err)
	}
	lane, err := findLane(plan, cfg.LaneName)
	if err != nil {
		fatalf("find lane: %v", err)
	}
	if strings.TrimSpace(cfg.AWSPrefix) == "" {
		cfg.AWSPrefix = lane.AWSPrefix
	}
	if err := cfg.validate(lane); err != nil {
		fatalf("validate config: %v", err)
	}

	rendered := renderToolConfig(cfg, lane)
	if err := os.WriteFile(cfg.OutputPath, []byte(rendered), 0o600); err != nil {
		fatalf("write %s: %v", cfg.OutputPath, err)
	}

	if cfg.EnvOutputPath != "" {
		env, err := renderEnvOutput(plan, lane)
		if err != nil {
			fatalf("render env output: %v", err)
		}
		if err := os.WriteFile(cfg.EnvOutputPath, []byte(env), 0o600); err != nil {
			fatalf("write %s: %v", cfg.EnvOutputPath, err)
		}
	}
}

func readPlan(path string) (signoffPlan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return signoffPlan{}, err
	}
	var plan signoffPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return signoffPlan{}, err
	}
	return plan, nil
}

func findLane(plan signoffPlan, laneName string) (signoffLane, error) {
	for _, lane := range plan.Lanes {
		if lane.Name == laneName {
			return lane, nil
		}
	}
	return signoffLane{}, fmt.Errorf("lane %q not found", laneName)
}

func (cfg renderConfig) validate(lane signoffLane) error {
	required := map[string]string{
		"bootstrap password":    cfg.BootstrapPassword,
		"lane install_rancher":  lane.InstallRancher,
		"aws region":            cfg.AWSRegion,
		"aws prefix":            cfg.AWSPrefix,
		"aws vpc":               cfg.AWSVPC,
		"aws subnet a":          cfg.AWSSubnetA,
		"aws subnet b":          cfg.AWSSubnetB,
		"aws subnet c":          cfg.AWSSubnetC,
		"aws ami":               cfg.AWSAMI,
		"aws subnet id":         cfg.AWSSubnetID,
		"aws security group id": cfg.AWSSecurityGroup,
		"aws pem key name":      cfg.AWSPemKeyName,
		"aws route53 fqdn":      cfg.AWSRoute53FQDN,
	}
	var missing []string
	for name, value := range required {
		if strings.TrimSpace(value) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required value(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

func renderToolConfig(cfg renderConfig, lane signoffLane) string {
	return fmt.Sprintf(`rancher:
  mode: auto
  version: %s
  distro: %s
  bootstrap_password: %s
  auto_approve: %t

rke2:
  preload_images: %t

total_has: 1

tf_vars:
  aws_region: %s
  aws_prefix: %s
  aws_vpc: %s
  aws_subnet_a: %s
  aws_subnet_b: %s
  aws_subnet_c: %s
  aws_ami: %s
  aws_subnet_id: %s
  aws_security_group_id: %s
  aws_pem_key_name: %s
  aws_route53_fqdn: %s
`,
		yamlQuote(strings.TrimPrefix(lane.InstallRancher, "v")),
		yamlQuote(cfg.RancherDistro),
		yamlQuote(cfg.BootstrapPassword),
		cfg.AutoApprove,
		cfg.PreloadImages,
		yamlQuote(cfg.AWSRegion),
		yamlQuote(cfg.AWSPrefix),
		yamlQuote(cfg.AWSVPC),
		yamlQuote(cfg.AWSSubnetA),
		yamlQuote(cfg.AWSSubnetB),
		yamlQuote(cfg.AWSSubnetC),
		yamlQuote(cfg.AWSAMI),
		yamlQuote(cfg.AWSSubnetID),
		yamlQuote(cfg.AWSSecurityGroup),
		yamlQuote(cfg.AWSPemKeyName),
		yamlQuote(cfg.AWSRoute53FQDN),
	)
}

func renderEnvOutput(plan signoffPlan, lane signoffLane) (string, error) {
	var b strings.Builder
	if err := writeGitHubEnvLine(&b, "TF_STATE_KEY", lane.TerraformStateKey); err != nil {
		return "", err
	}
	if err := writeGitHubEnvLine(&b, "SIGNOFF_AWS_PREFIX", lane.AWSPrefix); err != nil {
		return "", err
	}
	if err := writeGitHubEnvLine(&b, "RANCHER_UPGRADE_VERSION", lane.UpgradeToRancher); err != nil {
		return "", err
	}
	webhookImage := plan.WebhookImage
	if lane.WebhookOverrideImage != "" {
		webhookImage = lane.WebhookOverrideImage
	}
	if err := writeGitHubEnvLine(&b, "RANCHER_WEBHOOK_IMAGE", webhookImage); err != nil {
		return "", err
	}
	return b.String(), nil
}

func writeGitHubEnvLine(b *strings.Builder, key, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("%s contains a newline and cannot be written to GitHub env output", key)
	}
	fmt.Fprintf(b, "%s=%s\n", key, value)
	return nil
}

func yamlQuote(value string) string {
	return strconv.Quote(value)
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func fatalf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
