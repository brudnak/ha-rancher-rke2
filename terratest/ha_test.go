package test

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2Types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
	pricingTypes "github.com/aws/aws-sdk-go-v2/service/pricing/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/brudnak/ha-rancher-rke2/terratest/hcl"

	goversion "github.com/hashicorp/go-version"
	"github.com/gruntwork-io/terratest/modules/terraform"

	"github.com/spf13/viper"
	"golang.org/x/net/html"
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

func TestHaSetup(t *testing.T) {
	setupConfig(t)

	totalHAs := viper.GetInt("total_has")
	if totalHAs < 1 {
		t.Fatal("total_has must be at least 1")
	}

	resolvedPlans, err := prepareRancherConfiguration(totalHAs)
	if err != nil {
		t.Fatalf("Failed to prepare Rancher configuration: %v", err)
	}

	// Validate that the number of Helm commands matches the number of HA instances
	helmCommands := viper.GetStringSlice("rancher.helm_commands")
	if len(helmCommands) != totalHAs {
		t.Fatalf("Number of Helm commands (%d) does not match the number of HA instances (%d). Please ensure you have exactly %d Helm commands in your configuration.",
			len(helmCommands), totalHAs, totalHAs)
	}

	if err := validateLocalToolingPreflight(helmCommands); err != nil {
		t.Fatalf("Local tooling preflight failed before provisioning infrastructure: %v", err)
	}

	if err := validateSecretEnvironment(); err != nil {
		t.Fatalf("Secret environment preflight failed before provisioning infrastructure: %v", err)
	}

	if err := validatePinnedRKE2InstallerChecksum(resolvedPlans); err != nil {
		t.Fatalf("RKE2 installer checksum preflight failed before provisioning infrastructure: %v", err)
	}

	if err := confirmResolvedPlans(resolvedPlans); err != nil {
		t.Fatalf("Canceled before provisioning infrastructure: %v", err)
	}

	terraformOptions := getTerraformOptions(t, totalHAs)
	terraform.InitAndApply(t, terraformOptions)

	outputs := getTerraformOutputs(t, terraformOptions)
	if len(outputs) == 0 {
		t.Fatal("No outputs received from terraform")
	}

	// Process all HA instances in parallel
	var wg sync.WaitGroup
	var setupErr error
	var setupErrMutex sync.Mutex

	for i := 1; i <= totalHAs; i++ {
		wg.Add(1)
		instanceNum := i

		go func(instanceNum int) {
			defer wg.Done()

			log.Printf("Starting setup for HA instance %d", instanceNum)

			// Create a subtest instead of a custom wrapper
			t.Run(fmt.Sprintf("HA%d", instanceNum), func(subT *testing.T) {
				// We'll use a helper function that captures failures
				var resolvedPlan *RancherResolvedPlan
				if len(resolvedPlans) >= instanceNum {
					resolvedPlan = resolvedPlans[instanceNum-1]
				}
				if err := setupHAInstance(subT, instanceNum, outputs, resolvedPlan); err != nil {
					setupErrMutex.Lock()
					setupErr = fmt.Errorf("HA instance %d setup failed: %s", instanceNum, err.Error())
					setupErrMutex.Unlock()
					subT.Fail() // Mark the subtest as failed but continue execution
				}
			})
		}(instanceNum)
	}

	// Wait for all HA instances to complete setup
	wg.Wait()

	// Check if any errors occurred
	if setupErr != nil {
		t.Fatalf("Error during parallel HA setup: %v", setupErr)
	}

	logHASummary(totalHAs, outputs, resolvedPlans)
}

// setupHAInstance is a helper that returns errors instead of failing immediately
func setupHAInstance(t *testing.T, instanceNum int, outputs map[string]string, resolvedPlan *RancherResolvedPlan) error {
	haDir := fmt.Sprintf("high-availability-%d", instanceNum)

	haOutputs := getHAOutputs(instanceNum, outputs)

	// Validate IPs - return error instead of failing
	ips := []string{
		haOutputs.Server1IP, haOutputs.Server2IP, haOutputs.Server3IP,
		haOutputs.Server1PrivateIP, haOutputs.Server2PrivateIP, haOutputs.Server3PrivateIP,
	}
	for _, ip := range ips {
		if CheckIPAddress(ip) != "valid" {
			return fmt.Errorf("invalid IP address: %s", ip)
		}
	}

	// Get the absolute path to the current directory
	currentDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get current directory: %w", err)
	}

	// Make sure the HA directory exists using absolute path
	absHADir := filepath.Join(currentDir, haDir)
	if _, err := os.Stat(absHADir); os.IsNotExist(err) {
		// Try to create the directory if it doesn't exist
		if mkdirErr := os.MkdirAll(absHADir, os.ModePerm); mkdirErr != nil {
			return fmt.Errorf("failed to create directory %s: %w", absHADir, mkdirErr)
		}
		log.Printf("Created directory %s", absHADir)
	}

	// Get the Helm command for this instance (index is 0-based, instanceNum is 1-based)
	helmCommands := viper.GetStringSlice("rancher.helm_commands")
	helmCommand := helmCommands[instanceNum-1]

	// Inject the RancherURL into the Helm command by replacing the hostname
	// This assumes the hostname is specified with --set hostname=something
	if strings.Contains(helmCommand, "--set hostname=") {
		// Replace the hostname with the RancherURL from Terraform outputs
		helmCommand = strings.Replace(
			helmCommand,
			"--set hostname="+strings.Split(strings.Split(helmCommand, "--set hostname=")[1], " ")[0],
			"--set hostname="+haOutputs.RancherURL,
			1,
		)
	} else {
		// If no hostname is set, add it
		helmCommand = strings.TrimSpace(helmCommand) + fmt.Sprintf(" \\\n  --set hostname=%s", haOutputs.RancherURL)
	}

	// Create a Rancher installation script with the user-provided Helm command
	CreateInstallScript(helmCommand, haDir)

	// Setup first server node
	log.Printf("Setting up first server node with IP %s", haOutputs.Server1IP)
	err = setupFirstServerNode(haOutputs.Server1IP, haOutputs, resolvedPlan)
	if err != nil {
		return fmt.Errorf("failed to setup first server node: %w", err)
	}

	token, err := getNodeToken(haOutputs.Server1IP)
	if err != nil {
		return fmt.Errorf("failed to get node token: %w", err)
	}

	// Setup additional server nodes in parallel
	var wg sync.WaitGroup
	var setupErr error
	var setupErrMutex sync.Mutex

	for i, ip := range []string{haOutputs.Server2IP, haOutputs.Server3IP} {
		wg.Add(1)
		nodeNum := i + 2 // Node 2 or 3

		go func(ip string, nodeNum int) {
			defer wg.Done()

			log.Printf("Setting up server node %d with IP %s", nodeNum, ip)
			err := setupAdditionalServerNode(ip, token, haOutputs, resolvedPlan)
			if err != nil {
				setupErrMutex.Lock()
				setupErr = fmt.Errorf("failed to setup server node %d: %w", nodeNum, err)
				setupErrMutex.Unlock()
			}
		}(ip, nodeNum)
	}

	// Wait for all additional nodes to be set up
	wg.Wait()

	// Check if we got any errors during parallel setup
	if setupErr != nil {
		return fmt.Errorf("node setup error: %w", setupErr)
	}

	log.Printf("Waiting for cluster to fully initialize...")
	time.Sleep(30 * time.Second)

	// Get and save the kubeconfig with direct server IP
	err = getAndSaveKubeconfig(haOutputs.Server1IP, haDir)
	if err != nil {
		t.Logf("Warning: Failed to save kubeconfig: %v", err)
	}

	// Execute the install script now that the kubeconfig is available
	installScriptPath := fmt.Sprintf("%s/install.sh", haDir)
	log.Printf("Executing install script at %s", installScriptPath)

	// Get the absolute path to the current directory and the HA directory
	currentDir, dirErr := os.Getwd()
	if dirErr != nil {
		return fmt.Errorf("failed to get current directory: %w", dirErr)
	}

	// Make sure the HA directory exists
	absHADirForScript := filepath.Join(currentDir, haDir)
	if _, err := os.Stat(absHADirForScript); os.IsNotExist(err) {
		// Try to create the directory if it doesn't exist
		if mkdirErr := os.MkdirAll(absHADirForScript, os.ModePerm); mkdirErr != nil {
			return fmt.Errorf("failed to create directory %s: %w", absHADirForScript, mkdirErr)
		}
		log.Printf("Created directory %s", absHADirForScript)
	}

	// Use absolute paths for everything
	absInstallScriptPath := filepath.Join(absHADirForScript, "install.sh")
	absKubeConfigPath := filepath.Join(absHADirForScript, "kube_config.yaml")

	// Set up the command with the correct environment and working directory
	cmd := exec.Command(absInstallScriptPath)
	cmd.Dir = absHADirForScript
	cmd.Env = append(os.Environ(), fmt.Sprintf("KUBECONFIG=%s", absKubeConfigPath))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if execErr := cmd.Run(); execErr != nil {
		return fmt.Errorf("failed to execute install script: %w", execErr)
	}

	log.Printf("Install script executed successfully")

	log.Printf("HA %d setup complete", instanceNum)
	log.Printf("HA %d LB: %s", instanceNum, haOutputs.LoadBalancerDNS)
	log.Printf("HA %d Rancher URL: %s", instanceNum, clickableURL(haOutputs.RancherURL))

	return nil
}

func TestHACleanup(t *testing.T) {
	setupConfig(t)
	totalHAs := viper.GetInt("total_has")
	if err := validateSecretEnvironment(); err != nil {
		t.Fatalf("Secret environment preflight failed before cleanup: %v", err)
	}

	terraformOptions := getTerraformOptions(t, totalHAs)
	outputs := getTerraformOutputs(t, terraformOptions)
	costEstimate, estimateErr := estimateCurrentRunCost(totalHAs, outputs)
	if estimateErr != nil {
		log.Printf("[cleanup] Could not estimate EC2/EBS cost before destroy: %v", estimateErr)
	}
	terraform.Destroy(t, terraformOptions)

	for i := 1; i <= totalHAs; i++ {
		cleanupHAInstance(i)
	}
	cleanupTerraformFiles()

	if costEstimate != nil {
		logCleanupCostEstimate(costEstimate)
	}
}

// Global AWS clients (initialized once)
var (
	ssmClient *ssm.Client
	ec2Client *ec2.Client
)

// initAWSClients initializes AWS SDK clients
func initAWSClients() error {
	if ssmClient != nil {
		return nil // Already initialized
	}

	ctx := context.Background()

	// Get AWS region - try tf_vars first, then aws config
	region := viper.GetString("tf_vars.aws_region")
	if region == "" {
		region = viper.GetString("aws.region")
	}
	if region == "" {
		region = "us-east-2" // Default
	}

	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(
				os.Getenv("AWS_ACCESS_KEY_ID"),
				os.Getenv("AWS_SECRET_ACCESS_KEY"),
				"", // session token (empty for non-temporary credentials)
			),
		),
	)
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	ssmClient = ssm.NewFromConfig(cfg)
	ec2Client = ec2.NewFromConfig(cfg)

	log.Printf("AWS clients initialized for region: %s", region)
	return nil
}

// getInstanceIDFromIP finds the instance ID given a public IP address
func getInstanceIDFromIP(publicIP string) (string, error) {
	if err := initAWSClients(); err != nil {
		return "", err
	}

	ctx := context.Background()

	input := &ec2.DescribeInstancesInput{
		Filters: []ec2Types.Filter{
			{
				Name:   aws.String("ip-address"),
				Values: []string{publicIP},
			},
			{
				Name:   aws.String("instance-state-name"),
				Values: []string{"running"},
			},
		},
	}

	result, err := ec2Client.DescribeInstances(ctx, input)
	if err != nil {
		return "", fmt.Errorf("failed to describe instances: %w", err)
	}

	if len(result.Reservations) == 0 || len(result.Reservations[0].Instances) == 0 {
		return "", fmt.Errorf("no running instance found with IP %s", publicIP)
	}

	instanceID := aws.ToString(result.Reservations[0].Instances[0].InstanceId)
	log.Printf("Resolved IP %s to instance %s", publicIP, instanceID)

	return instanceID, nil
}

// waitForSSMAgent waits for SSM agent to be online
func waitForSSMAgent(instanceID string, maxSeconds int) error {
	if err := initAWSClients(); err != nil {
		return err
	}

	ctx := context.Background()

	log.Printf("Waiting for SSM agent on %s to be online...", instanceID)

	for i := 0; i < maxSeconds; i++ {
		input := &ssm.DescribeInstanceInformationInput{
			Filters: []types.InstanceInformationStringFilter{
				{
					Key:    aws.String("InstanceIds"),
					Values: []string{instanceID},
				},
			},
		}

		result, err := ssmClient.DescribeInstanceInformation(ctx, input)
		if err == nil && len(result.InstanceInformationList) > 0 {
			status := result.InstanceInformationList[0].PingStatus
			if status == types.PingStatusOnline {
				log.Printf("SSM agent is online for %s", instanceID)
				return nil
			}
		}

		if i%10 == 0 && i > 0 {
			log.Printf("Still waiting for SSM agent... (%d seconds)", i)
		}
		time.Sleep(1 * time.Second)
	}

	return fmt.Errorf("SSM agent did not come online after %d seconds", maxSeconds)
}

// runCommandSSM executes a command via SSM and returns the output
func runCommandSSM(cmd string, instanceID string) (string, error) {
	if err := initAWSClients(); err != nil {
		return "", err
	}

	ctx := context.Background()

	log.Printf("[SSM] Sending command to instance %s", instanceID)

	// Send command
	sendInput := &ssm.SendCommandInput{
		InstanceIds:  []string{instanceID},
		DocumentName: aws.String("AWS-RunShellScript"),
		Parameters: map[string][]string{
			"commands": {cmd},
		},
		TimeoutSeconds: aws.Int32(600),
	}

	sendOutput, err := ssmClient.SendCommand(ctx, sendInput)
	if err != nil {
		return "", fmt.Errorf("failed to send SSM command: %w", err)
	}

	commandID := sendOutput.Command.CommandId
	log.Printf("[SSM] Command sent with ID: %s", *commandID)

	// Wait for command to complete
	maxAttempts := 120 // 10 minutes (120 * 5 seconds)
	for i := 0; i < maxAttempts; i++ {
		time.Sleep(5 * time.Second)

		getInput := &ssm.GetCommandInvocationInput{
			CommandId:  commandID,
			InstanceId: aws.String(instanceID),
		}

		getOutput, err := ssmClient.GetCommandInvocation(ctx, getInput)
		if err != nil {
			// Command might not be ready yet
			continue
		}

		status := getOutput.Status

		switch status {
		case types.CommandInvocationStatusSuccess:
			output := aws.ToString(getOutput.StandardOutputContent)
			stderr := aws.ToString(getOutput.StandardErrorContent)

			// Keep a signal that stderr existed without dumping potentially sensitive content.
			if stderr != "" {
				log.Printf("[SSM] Command completed with stderr output (%d bytes)", len(stderr))
			}

			// Trim trailing newlines to match SSH behavior
			trimmedOutput := strings.TrimRight(output, "\r\n")
			log.Printf("[SSM] Command completed successfully. Output length: %d bytes", len(trimmedOutput))
			return trimmedOutput, nil

			case types.CommandInvocationStatusFailed,
				types.CommandInvocationStatusTimedOut,
				types.CommandInvocationStatusCancelled:
				stderr := aws.ToString(getOutput.StandardErrorContent)
				stdout := aws.ToString(getOutput.StandardOutputContent)
				log.Printf("[SSM] Command FAILED with status %s", status)
				log.Printf("[SSM] Failure output sizes: stdout=%d bytes stderr=%d bytes", len(stdout), len(stderr))
				if isRKE2InstallerChecksumFailure(stdout, stderr) {
					log.Printf("[SSM] SECURITY ERROR: RKE2 installer checksum validation failed on remote node")
					return "", fmt.Errorf("remote RKE2 installer checksum validation failed")
				}
				return "", fmt.Errorf("command failed with status %s", status)

		case types.CommandInvocationStatusInProgress,
			types.CommandInvocationStatusPending:
			// Still running
			if i%12 == 0 && i > 0 {
				log.Printf("[SSM] Command still running... (%d seconds)", i*5)
			}
			continue
		}
	}

	return "", fmt.Errorf("command timed out after %d attempts", maxAttempts)
}

// RunCommand is the drop-in replacement - same signature, uses SSM instead of SSH
func RunCommand(cmd string, pubIP string) (string, error) {
	log.Printf("[RunCommand] Starting command execution for IP %s", pubIP)

	// Get instance ID from IP
	instanceID, err := getInstanceIDFromIP(pubIP)
	if err != nil {
		return "", fmt.Errorf("failed to get instance ID from IP %s: %w", pubIP, err)
	}

	// Wait for SSM agent to be online (max 120 seconds for newly created instances)
	if err := waitForSSMAgent(instanceID, 120); err != nil {
		return "", fmt.Errorf("SSM agent not ready for instance %s: %w", instanceID, err)
	}

	// Execute command via SSM
	result, err := runCommandSSM(cmd, instanceID)
	if err != nil {
		log.Printf("[RunCommand] Command failed: %v", err)
		return "", err
	}

	log.Printf("[RunCommand] Command completed successfully")
	return result, nil
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

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

func confirmResolvedPlans(plans []*RancherResolvedPlan) error {
	if len(plans) == 0 {
		return nil
	}
	if plans[0] != nil && plans[0].Mode == "manual" {
		return nil
	}

	logResolvedPlans(plans)

	if viper.GetBool("rancher.auto_approve") {
		log.Printf("[resolver] Auto-approve enabled, continuing without prompt")
		return nil
	}

	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err == nil {
		defer tty.Close()

		if _, err := fmt.Fprint(tty, "Continue with this Rancher plan? [y/N]: "); err != nil {
			return fmt.Errorf("failed to write confirmation prompt: %w", err)
		}

		reader := bufio.NewReader(tty)
		response, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("failed to read confirmation response from terminal: %w", err)
		}

		switch strings.ToLower(strings.TrimSpace(response)) {
		case "y", "yes", "continue":
			log.Printf("[resolver] User approved resolved Rancher plans")
			return nil
		default:
			return fmt.Errorf("user canceled resolved Rancher plans")
		}
	}

	stdinInfo, err := os.Stdin.Stat()
	if err != nil {
		return fmt.Errorf("failed to inspect stdin for confirmation prompt: %w", err)
	}
	if stdinInfo.Mode()&os.ModeCharDevice == 0 {
		approved, err := confirmResolvedPlansWithOSDialog(plans)
		if err == nil {
			if approved {
				log.Printf("[resolver] User approved resolved Rancher plans")
				return nil
			}
			return fmt.Errorf("user canceled resolved Rancher plans")
		}
		return fmt.Errorf("confirmation prompt requires an interactive terminal; set rancher.auto_approve=true to skip it")
	}

	fmt.Print("Continue with this Rancher plan? [y/N]: ")
	reader := bufio.NewReader(os.Stdin)
	response, err := reader.ReadString('\n')
	if err != nil {
		approved, dialogErr := confirmResolvedPlansWithOSDialog(plans)
		if dialogErr == nil {
			if approved {
				log.Printf("[resolver] User approved resolved Rancher plans")
				return nil
			}
			return fmt.Errorf("user canceled resolved Rancher plans")
		}
		return fmt.Errorf("failed to read confirmation response: %w", err)
	}

	switch strings.ToLower(strings.TrimSpace(response)) {
	case "y", "yes", "continue":
		log.Printf("[resolver] User approved resolved Rancher plans")
		return nil
	default:
		return fmt.Errorf("user canceled resolved Rancher plans")
	}
}

func confirmResolvedPlansWithOSDialog(plans []*RancherResolvedPlan) (bool, error) {
	if runtime.GOOS != "darwin" {
		return false, fmt.Errorf("OS dialog confirmation is only supported on macOS")
	}

	script := `on run argv
set planMessage to item 1 of argv
button returned of (display dialog planMessage buttons {"Cancel", "Continue"} default button "Continue" cancel button "Cancel" with title "Rancher Plan Confirmation")
end run`
	output, err := exec.Command("osascript", "-e", script, buildResolvedPlansDialogMessage(plans)).CombinedOutput()
	if err != nil {
		return false, err
	}

	return strings.Contains(string(output), "Continue"), nil
}

func buildResolvedPlansDialogMessage(plans []*RancherResolvedPlan) string {
	sections := []string{"Continue with this Rancher plan?"}

	for i, plan := range plans {
		if plan == nil {
			continue
		}

		sectionLines := []string{
			fmt.Sprintf("HA %d", i+1),
		}
		if plan.RequestedVersion != "" {
			sectionLines = append(sectionLines, "Requested Rancher: "+plan.RequestedVersion)
		}
		if plan.ChartRepoAlias != "" && plan.ChartVersion != "" {
			sectionLines = append(sectionLines, fmt.Sprintf("Selected chart: %s/rancher@%s", plan.ChartRepoAlias, plan.ChartVersion))
		}
		if plan.RecommendedRKE2Version != "" {
			sectionLines = append(sectionLines, "Resolved RKE2/K8s: "+plan.RecommendedRKE2Version)
		}
		for commandIndex, helmCommand := range plan.HelmCommands {
			sectionLines = append(sectionLines, fmt.Sprintf("Helm command %d:", commandIndex+1))
			sectionLines = append(sectionLines, sanitizeHelmCommandForDialog(helmCommand))
		}

		sections = append(sections, strings.Join(sectionLines, "\n"))
	}

	return strings.Join(sections, "\n\n")
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

func logResolvedPlans(plans []*RancherResolvedPlan) {
	for i, plan := range plans {
		log.Printf("[resolver] Rancher resolution summary for HA %d:", i+1)
		log.Printf("[resolver] Requested version: %s", plan.RequestedVersion)
		log.Printf("[resolver] Requested distro: %s", plan.RequestedDistro)
		log.Printf("[resolver] Build type: %s", plan.BuildType)
		log.Printf("[resolver] Resolved distro: %s", plan.ResolvedDistro)
		log.Printf("[resolver] Chart repo: %s", plan.ChartRepoAlias)
		log.Printf("[resolver] Chart version: %s", plan.ChartVersion)
		log.Printf("[resolver] Rancher image: %s", plan.RancherImage)
		if plan.RancherImageTag != "" {
			log.Printf("[resolver] Rancher image tag: %s", plan.RancherImageTag)
		}
		if plan.AgentImage != "" {
			log.Printf("[resolver] Rancher agent image: %s", plan.AgentImage)
		}
		log.Printf("[resolver] Compatibility baseline: %s", plan.CompatibilityBaseline)
		log.Printf("[resolver] Support matrix: %s", plan.SupportMatrixURL)
		log.Printf("[resolver] Recommended RKE2 version: %s", plan.RecommendedRKE2Version)
		log.Printf("[resolver] Resolved installer SHA256: %s", plan.InstallerSHA256)
		for _, explanation := range plan.Explanation {
			log.Printf("[resolver] Reason: %s", explanation)
		}
		for commandIndex, helmCommand := range plan.HelmCommands {
			log.Printf("[resolver] Generated Helm command for HA %d.%d:\n%s", i+1, commandIndex+1, sanitizeHelmCommandForLog(helmCommand))
		}
	}
}

func sanitizeHelmCommandForLog(command string) string {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`bootstrapPassword=[^\s\\]+`),
		regexp.MustCompile(`dockerhub\.password=[^\s\\]+`),
	}

	sanitized := command
	for _, pattern := range patterns {
		sanitized = pattern.ReplaceAllStringFunc(sanitized, func(match string) string {
			parts := strings.SplitN(match, "=", 2)
			if len(parts) != 2 {
				return match
			}
			return parts[0] + "=<redacted>"
		})
	}
	return sanitized
}

func sanitizeHelmCommandForDialog(command string) string {
	sanitized := sanitizeHelmCommandForLog(command)
	return strings.TrimSpace(sanitized)
}

func clickableURL(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return value
	}
	if strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://") {
		return value
	}
	return "https://" + value
}

func resolveAutoRancherPlans(totalHAs int) ([]*RancherResolvedPlan, error) {
	requestedVersions, err := getRequestedRancherVersions(totalHAs)
	if err != nil {
		return nil, err
	}

	if err := refreshHelmRepoIndexes(); err != nil {
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
		if buildType != "release" && chartVersion == requestedVersion {
			rancherImage = ""
			rancherImageTag = ""
			agentImage = ""
			explanation = append(explanation, fmt.Sprintf("Using exact chart match %s/rancher@%s, so no Rancher image overrides are needed", chartRepoAlias, chartVersion))
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

		helmCommands := buildAutoHelmCommands(1, chartRepoAlias, chartVersion, bootstrapPassword, rancherImage, rancherImageTag, agentImage)

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
			return []string{"rancher-prime", "rancher-latest", "optimus-rancher-latest"}, "community-staging", []string{"Head build requested in auto mode, trying the latest released chart first and then falling back to community staging charts"}
		case "alpha":
			return []string{"rancher-prime", "rancher-latest", "optimus-rancher-alpha", "optimus-rancher-latest", "rancher-alpha"}, "community-staging", []string{"Alpha build requested in auto mode, trying the latest released chart first and then community alpha/staging chart sources"}
		case "rc":
			return []string{"rancher-prime", "rancher-latest", "optimus-rancher-latest"}, "community-staging", []string{"RC build requested in auto mode, trying the latest released chart first and then community staging chart sources"}
		default:
			return []string{"rancher-latest", "rancher-prime"}, "community", []string{"Released build requested in auto mode, trying community release sources first"}
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

type resolvedChartMatch struct {
	repoAlias             string
	chartVersion          string
	compatibilityBaseline string
	matchRank             int
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

func buildSupportMatrixURL(releasedVersion string) string {
	pathVersion := strings.ReplaceAll(releasedVersion, ".", "-")
	return fmt.Sprintf("https://www.suse.com/suse-rancher/support-matrix/all-supported-versions/rancher-v%s/", pathVersion)
}

func TestPreviousRancherMinorLine(t *testing.T) {
	previousMinorLine, err := previousRancherMinorLine("2.15")
	if err != nil {
		t.Fatalf("expected previous Rancher minor line, got error: %v", err)
	}

	if previousMinorLine != "2.14" {
		t.Fatalf("expected previous Rancher minor line 2.14, got %s", previousMinorLine)
	}
}

func TestFindLatestMinorReleaseIgnoresPrereleases(t *testing.T) {
	results := []helmSearchResult{
		{Version: "2.15.0-alpha3"},
		{Version: "2.14.1-rc1"},
		{Version: "2.14.1"},
		{Version: "2.14.0"},
	}

	version, err := findLatestMinorRelease(results, "2.14")
	if err != nil {
		t.Fatalf("expected released chart version, got error: %v", err)
	}

	if version != "2.14.1" {
		t.Fatalf("expected latest released 2.14.x chart version, got %s", version)
	}
}

func TestFindLatestMinorReleaseErrorsWithoutGA(t *testing.T) {
	results := []helmSearchResult{
		{Version: "2.15.0-alpha3"},
		{Version: "2.15.0-rc1"},
	}

	_, err := findLatestMinorRelease(results, "2.15")
	if err == nil {
		t.Fatal("expected an error when no released chart version exists")
	}
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

func buildAutoHelmCommands(totalHAs int, chartRepoAlias, chartVersion, bootstrapPassword, rancherImage, rancherImageTag, agentImage string) []string {
	baseSettings := []string{
		"helm install rancher " + chartRepoAlias + "/rancher \\",
		"  --namespace cattle-system \\",
		"  --version " + chartVersion + " \\",
		"  --set hostname=placeholder \\",
		"  --set bootstrapPassword=" + bootstrapPassword + " \\",
		"  --set ingress.tls.source=secret \\",
		"  --set global.cattle.psp.enabled=false \\",
		"  --set agentTLSMode=system-store",
	}

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

	command := strings.Join(baseSettings, "\n")
	commands := make([]string, totalHAs)
	for i := 0; i < totalHAs; i++ {
		commands[i] = command
	}
	return commands
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

// buildRKE2InstallCommand downloads the version-pinned RKE2 installer script,
// checks that it matches the pinned SHA256, and only then executes it.
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

func setupConfig(t *testing.T) {
	viper.AddConfigPath("../")
	viper.SetConfigName("tool-config")
	viper.SetConfigType("yml")

	if err := viper.ReadInConfig(); err != nil {
		t.Fatalf("Failed to read config: %v", err)
	}
}

func getTerraformOptions(t *testing.T, totalHAs int) *terraform.Options {
	generateAwsVars()

	return terraform.WithDefaultRetryableErrors(t, &terraform.Options{
		TerraformDir: "../modules/aws",
		NoColor:      true,
		Vars: map[string]interface{}{
			"total_has":             totalHAs,
			"aws_prefix":            viper.GetString("tf_vars.aws_prefix"),
			"aws_vpc":               viper.GetString("tf_vars.aws_vpc"),
			"aws_subnet_a":          viper.GetString("tf_vars.aws_subnet_a"),
			"aws_subnet_b":          viper.GetString("tf_vars.aws_subnet_b"),
			"aws_subnet_c":          viper.GetString("tf_vars.aws_subnet_c"),
			"aws_ami":               viper.GetString("tf_vars.aws_ami"),
			"aws_subnet_id":         viper.GetString("tf_vars.aws_subnet_id"),
			"aws_security_group_id": viper.GetString("tf_vars.aws_security_group_id"),
			"aws_pem_key_name":      viper.GetString("tf_vars.aws_pem_key_name"),
			"aws_route53_fqdn":      viper.GetString("tf_vars.aws_route53_fqdn"),
		},
	})
}

func generateAwsVars() {
	hcl.GenAwsVar(
		viper.GetString("tf_vars.aws_prefix"),
		viper.GetString("tf_vars.aws_vpc"),
		viper.GetString("tf_vars.aws_subnet_a"),
		viper.GetString("tf_vars.aws_subnet_b"),
		viper.GetString("tf_vars.aws_subnet_c"),
		viper.GetString("tf_vars.aws_ami"),
		viper.GetString("tf_vars.aws_subnet_id"),
		viper.GetString("tf_vars.aws_security_group_id"),
		viper.GetString("tf_vars.aws_pem_key_name"),
		viper.GetString("tf_vars.aws_route53_fqdn"),
	)
}

func getTerraformOutputs(t *testing.T, terraformOptions *terraform.Options) map[string]string {
	output := terraform.OutputJson(t, terraformOptions, "flat_outputs")

	var outputs map[string]string
	if err := json.Unmarshal([]byte(output), &outputs); err != nil {
		if t != nil {
			t.Logf("Raw output: %s", output)
			t.Fatalf("Failed to parse terraform outputs: %v", err)
		}
		log.Printf("Raw output: %s", output)
		return nil
	}

	return outputs
}

func getHAOutputs(instanceNum int, outputs map[string]string) TerraformOutputs {
	prefix := fmt.Sprintf("ha_%d", instanceNum)
	return TerraformOutputs{
		Server1IP:        outputs[fmt.Sprintf("%s_server1_ip", prefix)],
		Server2IP:        outputs[fmt.Sprintf("%s_server2_ip", prefix)],
		Server3IP:        outputs[fmt.Sprintf("%s_server3_ip", prefix)],
		Server1PrivateIP: outputs[fmt.Sprintf("%s_server1_private_ip", prefix)],
		Server2PrivateIP: outputs[fmt.Sprintf("%s_server2_private_ip", prefix)],
		Server3PrivateIP: outputs[fmt.Sprintf("%s_server3_private_ip", prefix)],
		LoadBalancerDNS:  outputs[fmt.Sprintf("%s_aws_lb", prefix)],
		RancherURL:       outputs[fmt.Sprintf("%s_rancher_url", prefix)],
	}
}

func logHASummary(totalHAs int, outputs map[string]string, resolvedPlans []*RancherResolvedPlan) {
	log.Printf("HA setup complete. Rancher URLs:")
	for i := 1; i <= totalHAs; i++ {
		haOutputs := getHAOutputs(i, outputs)
		requestedVersion := ""
		if len(resolvedPlans) >= i && resolvedPlans[i-1] != nil {
			requestedVersion = resolvedPlans[i-1].RequestedVersion
		}
		if requestedVersion != "" {
			log.Printf("Rancher instance %d (%s) -> %s", i, requestedVersion, clickableURL(haOutputs.RancherURL))
			continue
		}
		log.Printf("Rancher instance %d -> %s", i, clickableURL(haOutputs.RancherURL))
	}
}

func estimateCurrentRunCost(totalHAs int, outputs map[string]string) (*cleanupCostEstimate, error) {
	instanceIDs := make([]string, 0, totalHAs*3)
	seenIPs := map[string]bool{}

	for i := 1; i <= totalHAs; i++ {
		haOutputs := getHAOutputs(i, outputs)
		for _, ip := range []string{haOutputs.Server1IP, haOutputs.Server2IP, haOutputs.Server3IP} {
			if ip == "" || seenIPs[ip] {
				continue
			}
			seenIPs[ip] = true

			instanceID, err := getInstanceIDFromIP(ip)
			if err != nil {
				return nil, fmt.Errorf("failed to resolve instance ID for %s: %w", ip, err)
			}
			instanceIDs = append(instanceIDs, instanceID)
		}
	}

	if len(instanceIDs) == 0 {
		return nil, fmt.Errorf("no running instances found for cost estimate")
	}

	region := viper.GetString("tf_vars.aws_region")
	if region == "" {
		region = "us-east-2"
	}

	estimate, err := buildCleanupCostEstimate(region, instanceIDs)
	if err != nil {
		return nil, err
	}

	return estimate, nil
}

func buildCleanupCostEstimate(region string, instanceIDs []string) (*cleanupCostEstimate, error) {
	if err := initAWSClients(); err != nil {
		return nil, err
	}

	ctx := context.Background()
	describeOutput, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: instanceIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to describe instances for cleanup estimate: %w", err)
	}

	var instances []ec2Types.Instance
	for _, reservation := range describeOutput.Reservations {
		instances = append(instances, reservation.Instances...)
	}
	if len(instances) == 0 {
		return nil, fmt.Errorf("no instance details returned for cleanup estimate")
	}

	instanceType := string(instances[0].InstanceType)
	now := time.Now()
	totalRuntimeHours := 0.0
	volumeIDs := make([]string, 0, len(instances))
	seenVolumes := map[string]bool{}

	for _, instance := range instances {
		if string(instance.InstanceType) != instanceType {
			return nil, fmt.Errorf("mixed instance types are not yet supported in cleanup estimate")
		}
		if instance.LaunchTime != nil {
			totalRuntimeHours += now.Sub(*instance.LaunchTime).Hours()
		}
		for _, mapping := range instance.BlockDeviceMappings {
			if mapping.Ebs == nil || mapping.Ebs.VolumeId == nil {
				continue
			}
			volumeID := aws.ToString(mapping.Ebs.VolumeId)
			if volumeID == "" || seenVolumes[volumeID] {
				continue
			}
			seenVolumes[volumeID] = true
			volumeIDs = append(volumeIDs, volumeID)
		}
	}

	volumesOutput, err := ec2Client.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{
		VolumeIds: volumeIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to describe volumes for cleanup estimate: %w", err)
	}
	if len(volumesOutput.Volumes) == 0 {
		return nil, fmt.Errorf("no volume details returned for cleanup estimate")
	}

	volumeType := string(volumesOutput.Volumes[0].VolumeType)
	volumeSizeGiB := aws.ToInt32(volumesOutput.Volumes[0].Size)
	for _, volume := range volumesOutput.Volumes {
		if string(volume.VolumeType) != volumeType {
			return nil, fmt.Errorf("mixed EBS volume types are not yet supported in cleanup estimate")
		}
		if aws.ToInt32(volume.Size) != volumeSizeGiB {
			return nil, fmt.Errorf("mixed EBS volume sizes are not yet supported in cleanup estimate")
		}
	}

	ec2HourlyRateUSD, err := lookupEC2OnDemandHourlyPriceUSD(region, instanceType)
	if err != nil {
		return nil, err
	}

	ebsMonthlyRateUSD, err := lookupEBSMonthlyPricePerGiBUSD(region, volumeType)
	if err != nil {
		return nil, err
	}

	estimatedEC2CostUSD := ec2HourlyRateUSD * totalRuntimeHours
	estimatedEBSCostUSD := ebsMonthlyRateUSD * float64(volumeSizeGiB*int32(len(volumesOutput.Volumes))) * (totalRuntimeHours / 730.0)

	return &cleanupCostEstimate{
		Region:              region,
		TotalRuntimeHours:   totalRuntimeHours,
		InstanceCount:       len(instances),
		InstanceType:        instanceType,
		VolumeCount:         len(volumesOutput.Volumes),
		VolumeType:          volumeType,
		VolumeSizeGiB:       volumeSizeGiB,
		EC2HourlyRateUSD:    ec2HourlyRateUSD,
		EBSMonthlyRateUSD:   ebsMonthlyRateUSD,
		EstimatedEC2CostUSD: estimatedEC2CostUSD,
		EstimatedEBSCostUSD: estimatedEBSCostUSD,
	}, nil
}

func lookupEC2OnDemandHourlyPriceUSD(region, instanceType string) (float64, error) {
	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(
				os.Getenv("AWS_ACCESS_KEY_ID"),
				os.Getenv("AWS_SECRET_ACCESS_KEY"),
				"",
			),
		),
	)
	if err != nil {
		return 0, fmt.Errorf("failed to load AWS pricing config: %w", err)
	}

	pricingClient := pricing.NewFromConfig(cfg)
	location, err := awsPricingLocation(region)
	if err != nil {
		return 0, err
	}

	output, err := pricingClient.GetProducts(context.Background(), &pricing.GetProductsInput{
		ServiceCode: aws.String("AmazonEC2"),
		MaxResults:  aws.Int32(100),
		Filters: []pricingTypes.Filter{
			{Type: pricingTypes.FilterTypeTermMatch, Field: aws.String("location"), Value: aws.String(location)},
			{Type: pricingTypes.FilterTypeTermMatch, Field: aws.String("instanceType"), Value: aws.String(instanceType)},
			{Type: pricingTypes.FilterTypeTermMatch, Field: aws.String("operatingSystem"), Value: aws.String("Linux")},
			{Type: pricingTypes.FilterTypeTermMatch, Field: aws.String("tenancy"), Value: aws.String("Shared")},
			{Type: pricingTypes.FilterTypeTermMatch, Field: aws.String("preInstalledSw"), Value: aws.String("NA")},
			{Type: pricingTypes.FilterTypeTermMatch, Field: aws.String("capacitystatus"), Value: aws.String("Used")},
		},
	})
	if err != nil {
		return 0, fmt.Errorf("failed to query EC2 pricing: %w", err)
	}

	return extractUSDPriceFromPricingResult(output.PriceList)
}

func lookupEBSMonthlyPricePerGiBUSD(region, volumeType string) (float64, error) {
	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(
				os.Getenv("AWS_ACCESS_KEY_ID"),
				os.Getenv("AWS_SECRET_ACCESS_KEY"),
				"",
			),
		),
	)
	if err != nil {
		return 0, fmt.Errorf("failed to load AWS pricing config: %w", err)
	}

	pricingClient := pricing.NewFromConfig(cfg)
	location, err := awsPricingLocation(region)
	if err != nil {
		return 0, err
	}

	output, err := pricingClient.GetProducts(context.Background(), &pricing.GetProductsInput{
		ServiceCode: aws.String("AmazonEC2"),
		MaxResults:  aws.Int32(100),
		Filters: []pricingTypes.Filter{
			{Type: pricingTypes.FilterTypeTermMatch, Field: aws.String("location"), Value: aws.String(location)},
			{Type: pricingTypes.FilterTypeTermMatch, Field: aws.String("productFamily"), Value: aws.String("Storage")},
			{Type: pricingTypes.FilterTypeTermMatch, Field: aws.String("volumeApiName"), Value: aws.String(volumeType)},
		},
	})
	if err != nil {
		return 0, fmt.Errorf("failed to query EBS pricing: %w", err)
	}

	return extractUSDPriceFromPricingResult(output.PriceList)
}

func extractUSDPriceFromPricingResult(priceList []string) (float64, error) {
	type pricingDocument struct {
		Terms struct {
			OnDemand map[string]struct {
				PriceDimensions map[string]struct {
					PricePerUnit map[string]string `json:"pricePerUnit"`
				} `json:"priceDimensions"`
			} `json:"OnDemand"`
		} `json:"terms"`
	}

	bestPrice := math.MaxFloat64
	for _, item := range priceList {
		var doc pricingDocument
		if err := json.Unmarshal([]byte(item), &doc); err != nil {
			continue
		}

		for _, offer := range doc.Terms.OnDemand {
			for _, dimension := range offer.PriceDimensions {
				usdValue := strings.TrimSpace(dimension.PricePerUnit["USD"])
				if usdValue == "" {
					continue
				}
				var price float64
				if _, err := fmt.Sscanf(usdValue, "%f", &price); err != nil {
					continue
				}
				if price > 0 && price < bestPrice {
					bestPrice = price
				}
			}
		}
	}

	if bestPrice == math.MaxFloat64 {
		return 0, fmt.Errorf("no USD price found in pricing response")
	}

	return bestPrice, nil
}

func awsPricingLocation(region string) (string, error) {
	locations := map[string]string{
		"us-east-1": "US East (N. Virginia)",
		"us-east-2": "US East (Ohio)",
		"us-west-1": "US West (N. California)",
		"us-west-2": "US West (Oregon)",
	}
	location := locations[region]
	if location == "" {
		return "", fmt.Errorf("no AWS pricing location mapping configured for region %s", region)
	}
	return location, nil
}

func logCleanupCostEstimate(estimate *cleanupCostEstimate) {
	totalEstimatedUSD := estimate.EstimatedEC2CostUSD + estimate.EstimatedEBSCostUSD
	log.Printf("[cleanup] Estimated AWS cost for this run (EC2 + EBS only, live pricing):")
	log.Printf("[cleanup] Region: %s", estimate.Region)
	log.Printf("[cleanup] Total runtime across instances: %.2f hours", estimate.TotalRuntimeHours)
	log.Printf("[cleanup] EC2: %d x %s at $%.4f/hour -> $%.2f estimated",
		estimate.InstanceCount, estimate.InstanceType, estimate.EC2HourlyRateUSD, estimate.EstimatedEC2CostUSD)
	log.Printf("[cleanup] EBS: %d x %d GiB %s at $%.4f/GiB-month -> $%.2f estimated",
		estimate.VolumeCount, estimate.VolumeSizeGiB, estimate.VolumeType, estimate.EBSMonthlyRateUSD, estimate.EstimatedEBSCostUSD)
	log.Printf("[cleanup] Estimated total (EC2 + EBS only): $%.2f", totalEstimatedUSD)
}

func setupFirstServerNode(ip string, haOutputs TerraformOutputs, resolvedPlan *RancherResolvedPlan) error {
	log.Printf("[setupFirstServerNode] Starting setup for IP %s", ip)
	rke2K8sVersion := viper.GetString("k8s.version")
	expectedInstallerSHA256 := viper.GetString("rke2.install_script_sha256")
	if resolvedPlan != nil {
		rke2K8sVersion = resolvedPlan.RecommendedRKE2Version
		expectedInstallerSHA256 = resolvedPlan.InstallerSHA256
	}

	// Create config directory
	log.Printf("[setupFirstServerNode] Creating config directory...")
	cmd := "sudo mkdir -p /etc/rancher/rke2"
	output, err := RunCommand(cmd, ip)
	if err != nil {
		log.Printf("[setupFirstServerNode] FAILED to create config directory: %v", err)
		return fmt.Errorf("failed to create config directory: %w", err)
	}
	log.Printf("[setupFirstServerNode] Config directory created. Output: %s", output)

	configContent := fmt.Sprintf(`tls-san:
  - %s
  - %s
  - %s
  - %s
  - %s
  - %s
  - %s`,
		haOutputs.RancherURL,
		haOutputs.Server1IP,
		haOutputs.Server1PrivateIP,
		haOutputs.Server2IP,
		haOutputs.Server2PrivateIP,
		haOutputs.Server3IP,
		haOutputs.Server3PrivateIP)

	log.Printf("[setupFirstServerNode] Creating config file with content:\n%s", configContent)
	cmd = fmt.Sprintf("sudo bash -c 'cat > /etc/rancher/rke2/config.yaml << EOL\n%s\nEOL'", configContent)
	output, err = RunCommand(cmd, ip)
	if err != nil {
		log.Printf("[setupFirstServerNode] FAILED to create config file: %v", err)
		return fmt.Errorf("failed to create config file: %w", err)
	}
	log.Printf("[setupFirstServerNode] Config file created. Output: %s", output)

	// Verify config file was created
	log.Printf("[setupFirstServerNode] Verifying config file...")
	cmd = "sudo cat /etc/rancher/rke2/config.yaml"
	output, err = RunCommand(cmd, ip)
	if err != nil {
		log.Printf("[setupFirstServerNode] WARNING: Could not read config file: %v", err)
	} else {
		log.Printf("[setupFirstServerNode] Config file contents:\n%s", output)
	}

	// Check if we should pre-download RKE2 images to avoid Docker Hub rate limiting
	preloadImages := viper.GetBool("rke2.preload_images")

	if preloadImages {
		log.Printf("[setupFirstServerNode] Pre-downloading RKE2 images to avoid Docker Hub rate limiting...")

		// Create images directory
		cmd = "sudo mkdir -p /var/lib/rancher/rke2/agent/images"
		output, err = RunCommand(cmd, ip)
		if err != nil {
			log.Printf("[setupFirstServerNode] FAILED to create images directory: %v", err)
			return fmt.Errorf("failed to create images directory: %w", err)
		}
		log.Printf("[setupFirstServerNode] Images directory created")

		// Download images tarball
		imagesURL := fmt.Sprintf("https://github.com/rancher/rke2/releases/download/%s/rke2-images.linux-amd64.tar.zst", rke2K8sVersion)
		log.Printf("[setupFirstServerNode] Downloading images from %s (this may take a few minutes)...", imagesURL)
		cmd = fmt.Sprintf("curl -sfL %s -o /tmp/rke2-images.tar.zst", imagesURL)
		output, err = RunCommand(cmd, ip)
		if err != nil {
			log.Printf("[setupFirstServerNode] FAILED to download images: %v", err)
			return fmt.Errorf("failed to download RKE2 images: %w", err)
		}
		log.Printf("[setupFirstServerNode] Images downloaded successfully")

		// Move images to RKE2 directory
		cmd = "sudo mv /tmp/rke2-images.tar.zst /var/lib/rancher/rke2/agent/images/"
		output, err = RunCommand(cmd, ip)
		if err != nil {
			log.Printf("[setupFirstServerNode] FAILED to move images: %v", err)
			return fmt.Errorf("failed to move images: %w", err)
		}
		log.Printf("[setupFirstServerNode] Images pre-loaded successfully")
	} else {
		log.Printf("[setupFirstServerNode] Image pre-loading disabled, will pull from registry")
	}

	// Create registries.yaml with Docker Hub authentication if credentials are provided
	dockerUsername := strings.TrimSpace(os.Getenv("DOCKERHUB_USERNAME"))
	dockerPassword := strings.TrimSpace(os.Getenv("DOCKERHUB_PASSWORD"))

	if dockerUsername != "" && dockerPassword != "" {
		log.Printf("[setupFirstServerNode] Configuring Docker Hub authentication...")

		// Containerd requires base64 encoded "username:password" format
		authString := fmt.Sprintf("%s:%s", dockerUsername, dockerPassword)
		encodedAuth := base64.StdEncoding.EncodeToString([]byte(authString))

		registriesConfig := fmt.Sprintf(`configs:
  "registry-1.docker.io":
    auth:
      auth: %s
  "docker.io":
    auth:
      auth: %s`, encodedAuth, encodedAuth)

		cmd = fmt.Sprintf("sudo bash -c 'cat > /etc/rancher/rke2/registries.yaml << EOL\n%s\nEOL'", registriesConfig)
		output, err = RunCommand(cmd, ip)
		if err != nil {
			log.Printf("[setupFirstServerNode] FAILED to create registries.yaml: %v", err)
			return fmt.Errorf("failed to create registries.yaml: %w", err)
		}
		log.Printf("[setupFirstServerNode] Docker Hub authentication configured")
	} else {
		log.Printf("[setupFirstServerNode] No Docker Hub credentials provided, skipping registries.yaml creation")
	}

	// Install RKE2 server
	log.Printf("[setupFirstServerNode] Installing RKE2 version %s...", rke2K8sVersion)
	cmd, err = buildRKE2InstallCommand("server", rke2K8sVersion, expectedInstallerSHA256)
	if err != nil {
		return fmt.Errorf("failed to build RKE2 install command: %w", err)
	}
	output, err = RunCommand(cmd, ip)
	if err != nil {
		log.Printf("[setupFirstServerNode] FAILED to install RKE2: %v", err)
		log.Printf("[setupFirstServerNode] Install output: %s", output)
		return fmt.Errorf("failed to install RKE2: %w", err)
	}
	log.Printf("[setupFirstServerNode] RKE2 installed successfully. Output: %s", output)

	// Verify RKE2 binary exists
	log.Printf("[setupFirstServerNode] Verifying RKE2 binary...")
	cmd = "which rke2 || ls -la /usr/local/bin/rke2 || echo 'RKE2 binary not found in expected locations'"
	output, err = RunCommand(cmd, ip)
	log.Printf("[setupFirstServerNode] RKE2 binary check: %s", output)

	// Enable RKE2 server
	log.Printf("[setupFirstServerNode] Enabling RKE2 server service...")
	cmd = "sudo systemctl enable rke2-server.service"
	output, err = RunCommand(cmd, ip)
	if err != nil {
		log.Printf("[setupFirstServerNode] FAILED to enable RKE2 server: %v", err)
		return fmt.Errorf("failed to enable RKE2 server: %w", err)
	}
	log.Printf("[setupFirstServerNode] RKE2 server enabled. Output: %s", output)

	// Start RKE2 server
	log.Printf("[setupFirstServerNode] Starting RKE2 server service...")
	cmd = "sudo systemctl start rke2-server.service"
	output, err = RunCommand(cmd, ip)
	if err != nil {
		log.Printf("[setupFirstServerNode] FAILED to start RKE2 server: %v", err)

		// Get detailed error information
		log.Printf("[setupFirstServerNode] Gathering diagnostic information...")

		// Get service status
		cmd = "sudo systemctl status rke2-server.service --no-pager"
		statusOutput, statusErr := RunCommand(cmd, ip)
		if statusErr == nil {
			log.Printf("[setupFirstServerNode] Service status:\n%s", statusOutput)
		} else {
			log.Printf("[setupFirstServerNode] Could not get service status: %v", statusErr)
		}

		// Get recent logs
		cmd = "sudo journalctl -u rke2-server.service --no-pager -n 100"
		logsOutput, logsErr := RunCommand(cmd, ip)
		if logsErr == nil {
			log.Printf("[setupFirstServerNode] Recent logs:\n%s", logsOutput)
		} else {
			log.Printf("[setupFirstServerNode] Could not get logs: %v", logsErr)
		}

		return fmt.Errorf("failed to start RKE2 server: %w", err)
	}
	log.Printf("[setupFirstServerNode] RKE2 server start command completed. Output: %s", output)

	// Check service status immediately
	log.Printf("[setupFirstServerNode] Checking initial service status...")
	cmd = "sudo systemctl status rke2-server.service"
	output, _ = RunCommand(cmd, ip)
	log.Printf("[setupFirstServerNode] Service status:\n%s", output)

	// Wait for RKE2 to be ready by polling for the node-token file
	log.Printf("[setupFirstServerNode] Waiting for RKE2 to initialize on %s (this may take several minutes)...", ip)
	maxRetries := 30 // 5 minutes (30 * 10 seconds)
	for i := 0; i < maxRetries; i++ {
		log.Printf("[setupFirstServerNode] Attempt %d/%d: Checking for node-token file...", i+1, maxRetries)

		// Check if the node-token file exists
		cmd = "sudo test -f /var/lib/rancher/rke2/server/node-token && echo 'ready' || echo 'not-ready'"
		status, err := RunCommand(cmd, ip)
		log.Printf("[setupFirstServerNode] Node-token check result: '%s'", status)

		if err == nil && strings.TrimSpace(status) == "ready" {
			log.Printf("[setupFirstServerNode] RKE2 initialized successfully on %s", ip)

			// Verify we can actually read the token
			cmd = "sudo cat /var/lib/rancher/rke2/server/node-token"
			token, tokenErr := RunCommand(cmd, ip)
			if tokenErr != nil {
				log.Printf("[setupFirstServerNode] WARNING: Token file exists but cannot read it: %v", tokenErr)
			} else {
				log.Printf("[setupFirstServerNode] Token successfully read (length: %d)", len(token))
			}

			return nil
		}

		// Check service status for diagnostics
		if i%3 == 0 { // Check every 3 attempts (every 30 seconds)
			log.Printf("[setupFirstServerNode] Checking service status (attempt %d)...", i+1)
			cmd = "sudo systemctl status rke2-server.service --no-pager"
			statusOutput, _ := RunCommand(cmd, ip)
			log.Printf("[setupFirstServerNode] Service status:\n%s", statusOutput)

			// Check for any obvious errors in journalctl
			log.Printf("[setupFirstServerNode] Checking recent logs...")
			cmd = "sudo journalctl -u rke2-server.service --no-pager -n 20"
			logsOutput, _ := RunCommand(cmd, ip)
			log.Printf("[setupFirstServerNode] Recent logs:\n%s", logsOutput)
		}

		// Wait 10 seconds before checking again
		log.Printf("[setupFirstServerNode] Waiting 10 seconds before next check...")
		time.Sleep(10 * time.Second)
	}

	// Final diagnostic dump before giving up
	log.Printf("[setupFirstServerNode] TIMEOUT: Final diagnostic information:")

	cmd = "sudo systemctl status rke2-server.service --no-pager"
	output, _ = RunCommand(cmd, ip)
	log.Printf("[setupFirstServerNode] Final service status:\n%s", output)

	cmd = "sudo journalctl -u rke2-server.service --no-pager -n 50"
	output, _ = RunCommand(cmd, ip)
	log.Printf("[setupFirstServerNode] Last 50 log lines:\n%s", output)

	cmd = "sudo ls -la /var/lib/rancher/rke2/server/"
	output, _ = RunCommand(cmd, ip)
	log.Printf("[setupFirstServerNode] Contents of /var/lib/rancher/rke2/server/:\n%s", output)

	return fmt.Errorf("timeout waiting for RKE2 to initialize on %s", ip)
}

func getNodeToken(ip string) (string, error) {
	log.Printf("[getNodeToken] Retrieving node token from %s", ip)
	cmd := "sudo cat /var/lib/rancher/rke2/server/node-token"
	token, err := RunCommand(cmd, ip)
	if err != nil {
		log.Printf("[getNodeToken] FAILED to get node token: %v", err)
		return "", fmt.Errorf("failed to get node token: %w", err)
	}
	log.Printf("[getNodeToken] Token retrieved successfully (length: %d)", len(token))
	return token, nil
}

func setupAdditionalServerNode(ip, token string, haOutputs TerraformOutputs, resolvedPlan *RancherResolvedPlan) error {
	rke2K8sVersion := viper.GetString("k8s.version")
	expectedInstallerSHA256 := viper.GetString("rke2.install_script_sha256")
	if resolvedPlan != nil {
		rke2K8sVersion = resolvedPlan.RecommendedRKE2Version
		expectedInstallerSHA256 = resolvedPlan.InstallerSHA256
	}

	// Create config directory
	cmd := "sudo mkdir -p /etc/rancher/rke2"
	_, err := RunCommand(cmd, ip)
	if err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	configContent := fmt.Sprintf(`server: https://%s:9345
token: %s
tls-san:
  - %s
  - %s
  - %s
  - %s
  - %s
  - %s
  - %s`,
		haOutputs.Server1IP,
		token,
		haOutputs.RancherURL,
		haOutputs.Server1IP,
		haOutputs.Server1PrivateIP,
		haOutputs.Server2IP,
		haOutputs.Server2PrivateIP,
		haOutputs.Server3IP,
		haOutputs.Server3PrivateIP)

	cmd = fmt.Sprintf("sudo bash -c 'cat > /etc/rancher/rke2/config.yaml << EOL\n%s\nEOL'", configContent)
	_, err = RunCommand(cmd, ip)
	if err != nil {
		return fmt.Errorf("failed to create config file: %w", err)
	}

	// Check if we should pre-download RKE2 images to avoid Docker Hub rate limiting
	preloadImages := viper.GetBool("rke2.preload_images")

	if preloadImages {
		log.Printf("[setupAdditionalServerNode] Pre-downloading RKE2 images for %s...", ip)

		// Create images directory
		cmd = "sudo mkdir -p /var/lib/rancher/rke2/agent/images"
		_, err = RunCommand(cmd, ip)
		if err != nil {
			log.Printf("[setupAdditionalServerNode] FAILED to create images directory: %v", err)
			return fmt.Errorf("failed to create images directory: %w", err)
		}

		// Download images tarball
		imagesURL := fmt.Sprintf("https://github.com/rancher/rke2/releases/download/%s/rke2-images.linux-amd64.tar.zst", rke2K8sVersion)
		log.Printf("[setupAdditionalServerNode] Downloading images from %s...", imagesURL)
		cmd = fmt.Sprintf("curl -sfL %s -o /tmp/rke2-images.tar.zst", imagesURL)
		_, err = RunCommand(cmd, ip)
		if err != nil {
			log.Printf("[setupAdditionalServerNode] FAILED to download images: %v", err)
			return fmt.Errorf("failed to download RKE2 images: %w", err)
		}

		// Move images to RKE2 directory
		cmd = "sudo mv /tmp/rke2-images.tar.zst /var/lib/rancher/rke2/agent/images/"
		_, err = RunCommand(cmd, ip)
		if err != nil {
			log.Printf("[setupAdditionalServerNode] FAILED to move images: %v", err)
			return fmt.Errorf("failed to move images: %w", err)
		}
		log.Printf("[setupAdditionalServerNode] Images pre-loaded successfully for %s", ip)
	}

	// Create registries.yaml with Docker Hub authentication if credentials are provided
	dockerUsername := strings.TrimSpace(os.Getenv("DOCKERHUB_USERNAME"))
	dockerPassword := strings.TrimSpace(os.Getenv("DOCKERHUB_PASSWORD"))

	if dockerUsername != "" && dockerPassword != "" {
		log.Printf("[setupAdditionalServerNode] Configuring Docker Hub authentication for %s...", ip)

		// Containerd requires base64 encoded "username:password" format
		authString := fmt.Sprintf("%s:%s", dockerUsername, dockerPassword)
		encodedAuth := base64.StdEncoding.EncodeToString([]byte(authString))

		registriesConfig := fmt.Sprintf(`configs:
  "registry-1.docker.io":
    auth:
      auth: %s
  "docker.io":
    auth:
      auth: %s`, encodedAuth, encodedAuth)

		cmd = fmt.Sprintf("sudo bash -c 'cat > /etc/rancher/rke2/registries.yaml << EOL\n%s\nEOL'", registriesConfig)
		_, err = RunCommand(cmd, ip)
		if err != nil {
			log.Printf("[setupAdditionalServerNode] FAILED to create registries.yaml: %v", err)
			return fmt.Errorf("failed to create registries.yaml: %w", err)
		}
		log.Printf("[setupAdditionalServerNode] Docker Hub authentication configured for %s", ip)
	} else {
		log.Printf("[setupAdditionalServerNode] No Docker Hub credentials provided, skipping registries.yaml creation for %s", ip)
	}

	// Install RKE2 server
	log.Printf("[setupAdditionalServerNode] Installing RKE2 version %s on %s...", rke2K8sVersion, ip)
	cmd, err = buildRKE2InstallCommand("server", rke2K8sVersion, expectedInstallerSHA256)
	if err != nil {
		return fmt.Errorf("failed to build RKE2 install command: %w", err)
	}
	_, err = RunCommand(cmd, ip)
	if err != nil {
		return fmt.Errorf("failed to install RKE2: %w", err)
	}

	// Enable RKE2 server
	cmd = "sudo systemctl enable rke2-server.service"
	_, err = RunCommand(cmd, ip)
	if err != nil {
		return fmt.Errorf("failed to enable RKE2 server: %w", err)
	}

	cmd = "sudo systemctl start rke2-server.service"
	_, err = RunCommand(cmd, ip)
	if err != nil {
		return fmt.Errorf("failed to start RKE2 server: %w", err)
	}

	// Wait for RKE2 to be ready by checking kubelet status
	log.Printf("Waiting for RKE2 to initialize on %s (this may take several minutes)...", ip)
	maxRetries := 30 // 5 minutes (30 * 10 seconds)
	for i := 0; i < maxRetries; i++ {
		// Check if the kubelet is running, which indicates RKE2 has joined the cluster
		cmd = "sudo systemctl is-active --quiet rke2-server && echo 'active' || echo 'inactive'"
		status, err := RunCommand(cmd, ip)
		if err == nil && strings.TrimSpace(status) == "active" {
			log.Printf("RKE2 initialized successfully on %s", ip)
			return nil
		}

		// Wait 10 seconds before checking again
		time.Sleep(10 * time.Second)
	}

	return fmt.Errorf("timeout waiting for RKE2 to initialize on %s", ip)
}

func getAndSaveKubeconfig(serverIP string, haDir string) error {
	// Get the kubeconfig from the server
	rawKubeconfig, err := RunCommand("sudo cat /etc/rancher/rke2/rke2.yaml", serverIP)
	if err != nil {
		return fmt.Errorf("failed to retrieve kubeconfig: %w", err)
	}

	// Replace the local IP with the server's public IP
	configIP := fmt.Sprintf("https://%s:6443", serverIP)
	modifiedKubeconfig := strings.Replace(rawKubeconfig, "https://127.0.0.1:6443", configIP, -1)

	// Get the absolute path to the current directory
	currentDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get current directory: %w", err)
	}

	// Make sure the HA directory exists
	absHADir := filepath.Join(currentDir, haDir)
	if _, err := os.Stat(absHADir); os.IsNotExist(err) {
		// Try to create the directory if it doesn't exist
		if mkdirErr := os.MkdirAll(absHADir, os.ModePerm); mkdirErr != nil {
			return fmt.Errorf("failed to create directory %s: %w", absHADir, mkdirErr)
		}
		log.Printf("Created directory %s", absHADir)
	}

	// Write the modified kubeconfig to the high-availability folder using absolute path
	absKubeConfigPath := filepath.Join(absHADir, "kube_config.yaml")
	err = os.WriteFile(absKubeConfigPath, []byte(modifiedKubeconfig), 0644)
	if err != nil {
		return fmt.Errorf("failed to write kubeconfig file: %w", err)
	}

	log.Printf("Kubeconfig saved to %s", absKubeConfigPath)
	return nil
}

func cleanupHAInstance(instanceNum int) {
	haDir := fmt.Sprintf("high-availability-%d", instanceNum)

	filesToRemove := []string{
		fmt.Sprintf("%s/install.sh", haDir),
		fmt.Sprintf("%s/kube_config.yaml", haDir),
		fmt.Sprintf("%s/kube_config_lb.yaml", haDir),
	}

	for _, file := range filesToRemove {
		RemoveFile(file)
	}

	RemoveFolder(haDir)
}

func cleanupTerraformFiles() {
	files := []string{
		"../modules/aws/.terraform.lock.hcl",
		"../modules/aws/terraform.tfstate",
		"../modules/aws/terraform.tfstate.backup",
		"../modules/aws/terraform.tfvars",
	}

	for _, file := range files {
		RemoveFile(file)
	}

	RemoveFolder("../modules/aws/.terraform")
}

func CreateInstallScript(helmCommand, haDir string) {
	installScript := fmt.Sprintf(`#!/bin/bash
# First make sure we're using the right kubeconfig
if [ ! -f "kube_config.yaml" ]; then
  echo "ERROR: kube_config.yaml not found. Make sure you're in the right directory."
  exit 1
fi

# Export KUBECONFIG to point to our kubeconfig file
export KUBECONFIG=$(pwd)/kube_config.yaml

# Verify kubectl can connect to the cluster
echo "Verifying connection to Kubernetes cluster..."
kubectl cluster-info
if [ $? -ne 0 ]; then
  echo "ERROR: Unable to connect to Kubernetes cluster. Check your kubeconfig."
  exit 1
fi

helm repo update

echo "Creating namespace..."
kubectl create namespace cattle-system

echo "Installing Rancher..."
%s

echo "Rancher installation complete!"`, helmCommand)

	// Get the absolute path to the current directory
	currentDir, err := os.Getwd()
	if err != nil {
		log.Printf("Failed to get current directory: %v", err)
		return
	}

	// Make sure the HA directory exists
	absHADir := filepath.Join(currentDir, haDir)
	if _, err := os.Stat(absHADir); os.IsNotExist(err) {
		// Try to create the directory if it doesn't exist
		if mkdirErr := os.MkdirAll(absHADir, os.ModePerm); mkdirErr != nil {
			log.Printf("Failed to create directory %s: %v", absHADir, mkdirErr)
			return
		}
		log.Printf("Created directory %s", absHADir)
	}

	// Use absolute path for the install script
	absInstallScriptPath := filepath.Join(absHADir, "install.sh")
	writeFile(absInstallScriptPath, []byte(installScript))

	// Make the script executable
	err = os.Chmod(absInstallScriptPath, 0755)
	if err != nil {
		log.Printf("Failed to make script executable: %v", err)
		return
	}
}

func writeFile(path string, data []byte) {
	if err := os.WriteFile(path, data, 0644); err != nil {
		log.Printf("Failed to write file %s: %v", path, err)
	}
}

func CheckIPAddress(ip string) string {
	if net.ParseIP(ip) == nil {
		return "invalid"
	}
	return "valid"
}

func RemoveFile(filePath string) {
	if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
		log.Printf("Failed to remove file %s: %v", filePath, err)
	}
}

func CreateDir(path string) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(path, os.ModePerm); err != nil {
			log.Printf("Failed to create directory %s: %v", path, err)
		}
	}
}

func RemoveFolder(folderPath string) {
	if err := os.RemoveAll(folderPath); err != nil {
		log.Printf("Failed to remove folder %s: %v", folderPath, err)
	}
}
