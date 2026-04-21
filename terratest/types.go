package test

import (
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

type TerraformOutputs struct {
	Server1IP        string
	Server2IP        string
	Server3IP        string
	Server1PrivateIP string
	Server2PrivateIP string
	Server3PrivateIP string
	LoadBalancerDNS  string
	RancherURL       string
}

type RancherResolvedPlan struct {
	Mode                   string
	RequestedVersion       string
	RequestedDistro        string
	BuildType              string
	ResolvedDistro         string
	ChartRepoAlias         string
	ChartVersion           string
	RancherImage           string
	RancherImageTag        string
	AgentImage             string
	CompatibilityBaseline  string
	SupportMatrixURL       string
	RecommendedRKE2Version string
	InstallerSHA256        string
	HelmCommands           []string
	Explanation            []string
}

type helmSearchResult struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	AppVersion  string `json:"app_version"`
	Description string `json:"description"`
}

type cleanupCostEstimate struct {
	Region              string
	TotalRuntimeHours   float64
	InstanceCount       int
	InstanceType        string
	VolumeCount         int
	VolumeType          string
	VolumeSizeGiB       int32
	EC2HourlyRateUSD    float64
	EBSMonthlyRateUSD   float64
	EstimatedEC2CostUSD float64
	EstimatedEBSCostUSD float64
}

type resolvedChartMatch struct {
	repoAlias             string
	chartVersion          string
	compatibilityBaseline string
	matchRank             int
}

var (
	ssmClient *ssm.Client
	ec2Client *ec2.Client
)
