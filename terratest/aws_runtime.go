package test

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"strings"
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
	"github.com/spf13/viper"
)

func initAWSClients() error {
	if ssmClient != nil {
		return nil
	}

	ctx := context.Background()

	region := viper.GetString("tf_vars.aws_region")
	if region == "" {
		region = viper.GetString("aws.region")
	}
	if region == "" {
		region = "us-east-2"
	}

	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(
				os.Getenv("AWS_ACCESS_KEY_ID"),
				os.Getenv("AWS_SECRET_ACCESS_KEY"),
				"",
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

func runCommandSSM(cmd string, instanceID string) (string, error) {
	if err := initAWSClients(); err != nil {
		return "", err
	}

	ctx := context.Background()
	log.Printf("[SSM] Sending command to instance %s", instanceID)

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

	maxAttempts := 120
	for i := 0; i < maxAttempts; i++ {
		time.Sleep(5 * time.Second)

		getInput := &ssm.GetCommandInvocationInput{
			CommandId:  commandID,
			InstanceId: aws.String(instanceID),
		}

		getOutput, err := ssmClient.GetCommandInvocation(ctx, getInput)
		if err != nil {
			continue
		}

		status := getOutput.Status

		switch status {
		case types.CommandInvocationStatusSuccess:
			output := aws.ToString(getOutput.StandardOutputContent)
			stderr := aws.ToString(getOutput.StandardErrorContent)

			if stderr != "" {
				log.Printf("[SSM] Command completed with stderr output (%d bytes)", len(stderr))
			}

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
			if i%12 == 0 && i > 0 {
				log.Printf("[SSM] Command still running... (%d seconds)", i*5)
			}
			continue
		}
	}

	return "", fmt.Errorf("command timed out after %d attempts", maxAttempts)
}

func RunCommand(cmd string, pubIP string) (string, error) {
	log.Printf("[RunCommand] Starting command execution for IP %s", pubIP)

	instanceID, err := getInstanceIDFromIP(pubIP)
	if err != nil {
		return "", fmt.Errorf("failed to get instance ID from IP %s: %w", pubIP, err)
	}

	if err := waitForSSMAgent(instanceID, 120); err != nil {
		return "", fmt.Errorf("SSM agent not ready for instance %s: %w", instanceID, err)
	}

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

	return buildCleanupCostEstimate(region, instanceIDs)
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
