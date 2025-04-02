package test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/brudnak/ha-rancher-rke2/terratest/hcl"

	"github.com/gruntwork-io/terratest/modules/terraform"
	"github.com/stretchr/testify/assert"
	"golang.org/x/crypto/ssh"

	"github.com/spf13/viper"
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

func TestHaSetup(t *testing.T) {
	setupConfig(t)

	totalHAs := viper.GetInt("total_has")
	if totalHAs < 1 {
		t.Fatal("total_has must be at least 1")
	}

	terraformOptions := getTerraformOptions(t, totalHAs)
	terraform.InitAndApply(t, terraformOptions)

	outputs := getTerraformOutputs(t, terraformOptions)
	if len(outputs) == 0 {
		t.Fatal("No outputs received from terraform")
	}

	for i := 1; i <= totalHAs; i++ {
		processHAInstance(t, i, outputs)
	}
}

func TestHACleanup(t *testing.T) {
	setupConfig(t)
	totalHAs := viper.GetInt("total_has")

	terraformOptions := getTerraformOptions(t, totalHAs)
	terraform.Destroy(t, terraformOptions)

	for i := 1; i <= totalHAs; i++ {
		cleanupHAInstance(i)
	}
	cleanupTerraformFiles()
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
			"aws_access_key":        viper.GetString("tf_vars.aws_access_key"),
			"aws_secret_key":        viper.GetString("tf_vars.aws_secret_key"),
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
		viper.GetString("tf_vars.aws_access_key"),
		viper.GetString("tf_vars.aws_secret_key"),
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
		t.Logf("Raw output: %s", output)
		t.Fatalf("Failed to parse terraform outputs: %v", err)
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

func RunCommand(cmd string, pubIP string) (string, error) {
	pemKey := viper.GetString("aws.rsa_private_key")
	dialIP := fmt.Sprintf("%s:22", pubIP)
	signer, err := ssh.ParsePrivateKey([]byte(pemKey))
	if err != nil {
		return "", fmt.Errorf("failed to parse private key: %w", err)
	}

	config := &ssh.ClientConfig{
		User:            "ubuntu",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	conn, err := ssh.Dial("tcp", dialIP, config)
	if err != nil {
		return "", fmt.Errorf("failed to establish ssh connection: %w", err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			log.Println(err)
		}
	}()

	session, err := conn.NewSession()
	if err != nil {
		return "", fmt.Errorf("failed to create new ssh session: %w", err)
	}
	defer func() {
		if err := session.Close(); err != nil {
			log.Println(err)
		}
	}()

	var stdoutBuf bytes.Buffer
	session.Stdout = &stdoutBuf
	err = session.Run(cmd)
	if err != nil {
		return "", fmt.Errorf("failed to run ssh command: %w", err)
	}

	stringOut := stdoutBuf.String()
	stringOut = strings.TrimRight(stringOut, "\r\n")

	return stringOut, nil
}

func processHAInstance(t *testing.T, instanceNum int, outputs map[string]string) {
	haDir := fmt.Sprintf("high-availability-%d", instanceNum)

	haOutputs := getHAOutputs(instanceNum, outputs)
	validateIPs(t, haOutputs)

	CreateDir(haDir)

	// Create Rancher installation script
	bootstrapPassword := viper.GetString("rancher.bootstrap_password")
	image := viper.GetString("rancher.image_tag")
	chart := viper.GetString("rancher.version")
	CreateInstallScript(haOutputs.RancherURL, bootstrapPassword, image, chart, haDir)

	// Setup first server node
	log.Printf("Setting up first server node with IP %s", haOutputs.Server1IP)
	err := setupFirstServerNode(haOutputs.Server1IP, haOutputs)
	if err != nil {
		t.Fatalf("Failed to setup first server node: %v", err)
	}

	// Get token from first node
	token, err := getNodeToken(haOutputs.Server1IP)
	if err != nil {
		t.Fatalf("Failed to get node token: %v", err)
	}

	// Setup additional server nodes
	log.Printf("Setting up second server node with IP %s", haOutputs.Server2IP)
	err = setupAdditionalServerNode(haOutputs.Server2IP, token, haOutputs)
	if err != nil {
		t.Fatalf("Failed to setup second server node: %v", err)
	}

	log.Printf("Setting up third server node with IP %s", haOutputs.Server3IP)
	err = setupAdditionalServerNode(haOutputs.Server3IP, token, haOutputs)
	if err != nil {
		t.Fatalf("Failed to setup third server node: %v", err)
	}

	// Wait for cluster to fully initialize (all nodes ready)
	log.Printf("Waiting for cluster to fully initialize...")
	time.Sleep(30 * time.Second)

	// Get and save the kubeconfig with direct server IP
	err = getAndSaveKubeconfig(haOutputs.Server1IP, haDir)
	if err != nil {
		t.Logf("Warning: Failed to save kubeconfig: %v", err)
	}

	log.Printf("HA %d setup complete", instanceNum)
	log.Printf("HA %d LB: %s", instanceNum, haOutputs.LoadBalancerDNS)
	log.Printf("HA %d Rancher URL: %s", instanceNum, haOutputs.RancherURL)
}

func setupFirstServerNode(ip string, haOutputs TerraformOutputs) error {
	// Create config directory
	cmd := "sudo mkdir -p /etc/rancher/rke2"
	_, err := RunCommand(cmd, ip)
	if err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	// Create config file with TLS SAN entries for all nodes and load balancer
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

	cmd = fmt.Sprintf("sudo bash -c 'cat > /etc/rancher/rke2/config.yaml << EOL\n%s\nEOL'", configContent)
	_, err = RunCommand(cmd, ip)
	if err != nil {
		return fmt.Errorf("failed to create config file: %w", err)
	}

	// Install RKE2 server
	rke2K8sVersion := viper.GetString("k8s.version")
	// Fixed command
	cmd = fmt.Sprintf(`sudo sh -c 'curl -sfL https://get.rke2.io | INSTALL_RKE2_VERSION=%s INSTALL_RKE2_TYPE=server sh -'`, rke2K8sVersion)
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

	// Start RKE2 server in the background
	cmd = "sudo systemctl start rke2-server.service &"
	_, err = RunCommand(cmd, ip)
	if err != nil {
		return fmt.Errorf("failed to start RKE2 server: %w", err)
	}

	// Wait for RKE2 to be ready by polling for the node-token file
	log.Printf("Waiting for RKE2 to initialize on %s (this may take several minutes)...", ip)
	maxRetries := 30 // 5 minutes (30 * 10 seconds)
	for i := 0; i < maxRetries; i++ {
		// Check if the node-token file exists, which indicates RKE2 is initialized
		cmd = "sudo test -f /var/lib/rancher/rke2/server/node-token && echo 'ready' || echo 'not-ready'"
		status, err := RunCommand(cmd, ip)
		if err == nil && strings.TrimSpace(status) == "ready" {
			log.Printf("RKE2 initialized successfully on %s", ip)
			return nil
		}

		// Wait 10 seconds before checking again
		time.Sleep(10 * time.Second)
	}

	return fmt.Errorf("timeout waiting for RKE2 to initialize on %s", ip)
}

func getNodeToken(ip string) (string, error) {
	cmd := "sudo cat /var/lib/rancher/rke2/server/node-token"
	token, err := RunCommand(cmd, ip)
	if err != nil {
		return "", fmt.Errorf("failed to get node token: %w", err)
	}
	return token, nil
}

func setupAdditionalServerNode(ip, token string, haOutputs TerraformOutputs) error {
	// Create config directory
	cmd := "sudo mkdir -p /etc/rancher/rke2"
	_, err := RunCommand(cmd, ip)
	if err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	// Create config file with server URL, token, and TLS SAN entries for all nodes and load balancer
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

	// Install RKE2 server
	rke2K8sVersion := viper.GetString("k8s.version")
	cmd = fmt.Sprintf(`sudo sh -c 'curl -sfL https://get.rke2.io | INSTALL_RKE2_VERSION=%s INSTALL_RKE2_TYPE=server sh -'`, rke2K8sVersion)
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

	// Start RKE2 server in the background
	cmd = "sudo systemctl start rke2-server.service &"
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

	// Write the modified kubeconfig to the high-availability folder
	kubeConfigPath := fmt.Sprintf("%s/kube_config.yaml", haDir)
	err = os.WriteFile(kubeConfigPath, []byte(modifiedKubeconfig), 0644)
	if err != nil {
		return fmt.Errorf("failed to write kubeconfig file: %w", err)
	}

	log.Printf("Kubeconfig saved to %s", kubeConfigPath)
	return nil
}

func validateIPs(t *testing.T, outputs TerraformOutputs) {
	ips := []string{
		outputs.Server1IP, outputs.Server2IP, outputs.Server3IP,
		outputs.Server1PrivateIP, outputs.Server2PrivateIP, outputs.Server3PrivateIP,
	}

	for _, ip := range ips {
		assert.Equal(t, "valid", CheckIPAddress(ip), fmt.Sprintf("Invalid IP address: %s", ip))
	}
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

func CreateInstallScript(rancherURL, bsPassword, image, chart, haDir string) {
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
helm install rancher rancher-latest/rancher \
  --namespace cattle-system \
  --set hostname=%s \
  --set bootstrapPassword=%s \
  --set tls=external \
  --set global.cattle.psp.enabled=false \
  --set rancherImageTag=%s \
  --version %s \
  --set agentTLSMode=system-store

echo "Rancher installation complete!"`, rancherURL, bsPassword, image, chart)

	writeFile(fmt.Sprintf("%s/install.sh", haDir), []byte(installScript))

	// Make the script executable
	err := os.Chmod(fmt.Sprintf("%s/install.sh", haDir), 0755)
	if err != nil {
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
