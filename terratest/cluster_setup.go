package test

import (
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/viper"
)

func setupHAInstance(t *testing.T, instanceNum int, outputs map[string]string, resolvedPlan *RancherResolvedPlan) error {
	haDir := fmt.Sprintf("high-availability-%d", instanceNum)
	haOutputs := getHAOutputs(instanceNum, outputs)

	ips := []string{
		haOutputs.Server1IP, haOutputs.Server2IP, haOutputs.Server3IP,
		haOutputs.Server1PrivateIP, haOutputs.Server2PrivateIP, haOutputs.Server3PrivateIP,
	}
	for _, ip := range ips {
		if CheckIPAddress(ip) != "valid" {
			return fmt.Errorf("invalid IP address: %s", ip)
		}
	}

	currentDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get current directory: %w", err)
	}

	absHADir := filepath.Join(currentDir, haDir)
	if _, err := os.Stat(absHADir); os.IsNotExist(err) {
		if mkdirErr := os.MkdirAll(absHADir, os.ModePerm); mkdirErr != nil {
			return fmt.Errorf("failed to create directory %s: %w", absHADir, mkdirErr)
		}
		log.Printf("Created directory %s", absHADir)
	}

	helmCommands := viper.GetStringSlice("rancher.helm_commands")
	helmCommand := helmCommands[instanceNum-1]

	if strings.Contains(helmCommand, "--set hostname=") {
		helmCommand = strings.Replace(
			helmCommand,
			"--set hostname="+strings.Split(strings.Split(helmCommand, "--set hostname=")[1], " ")[0],
			"--set hostname="+haOutputs.RancherURL,
			1,
		)
	} else {
		helmCommand = strings.TrimSpace(helmCommand) + fmt.Sprintf(" \\\n  --set hostname=%s", haOutputs.RancherURL)
	}

	CreateInstallScript(helmCommand, haDir)

	log.Printf("Setting up first server node with IP %s", haOutputs.Server1IP)
	err = setupFirstServerNode(haOutputs.Server1IP, haOutputs, resolvedPlan)
	if err != nil {
		return fmt.Errorf("failed to setup first server node: %w", err)
	}

	token, err := getNodeToken(haOutputs.Server1IP)
	if err != nil {
		return fmt.Errorf("failed to get node token: %w", err)
	}

	var wg sync.WaitGroup
	var setupErr error
	var setupErrMutex sync.Mutex

	for i, ip := range []string{haOutputs.Server2IP, haOutputs.Server3IP} {
		wg.Add(1)
		nodeNum := i + 2

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

	wg.Wait()

	if setupErr != nil {
		return fmt.Errorf("node setup error: %w", setupErr)
	}

	log.Printf("Waiting for cluster to fully initialize...")
	time.Sleep(30 * time.Second)

	err = getAndSaveKubeconfig(haOutputs.Server1IP, haDir)
	if err != nil {
		t.Logf("Warning: Failed to save kubeconfig: %v", err)
	}

	installScriptPath := fmt.Sprintf("%s/install.sh", haDir)
	log.Printf("Executing install script at %s", installScriptPath)

	currentDir, dirErr := os.Getwd()
	if dirErr != nil {
		return fmt.Errorf("failed to get current directory: %w", dirErr)
	}

	absHADirForScript := filepath.Join(currentDir, haDir)
	if _, err := os.Stat(absHADirForScript); os.IsNotExist(err) {
		if mkdirErr := os.MkdirAll(absHADirForScript, os.ModePerm); mkdirErr != nil {
			return fmt.Errorf("failed to create directory %s: %w", absHADirForScript, mkdirErr)
		}
		log.Printf("Created directory %s", absHADirForScript)
	}

	absInstallScriptPath := filepath.Join(absHADirForScript, "install.sh")
	absKubeConfigPath := filepath.Join(absHADirForScript, "kube_config.yaml")

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

func setupFirstServerNode(ip string, haOutputs TerraformOutputs, resolvedPlan *RancherResolvedPlan) error {
	log.Printf("[setupFirstServerNode] Starting setup for IP %s", ip)
	rke2K8sVersion := viper.GetString("k8s.version")
	expectedInstallerSHA256 := viper.GetString("rke2.install_script_sha256")
	if resolvedPlan != nil {
		rke2K8sVersion = resolvedPlan.RecommendedRKE2Version
		expectedInstallerSHA256 = resolvedPlan.InstallerSHA256
	}

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

	log.Printf("[setupFirstServerNode] Verifying config file...")
	cmd = "sudo cat /etc/rancher/rke2/config.yaml"
	output, err = RunCommand(cmd, ip)
	if err != nil {
		log.Printf("[setupFirstServerNode] WARNING: Could not read config file: %v", err)
	} else {
		log.Printf("[setupFirstServerNode] Config file contents:\n%s", output)
	}

	preloadImages := viper.GetBool("rke2.preload_images")

	if preloadImages {
		log.Printf("[setupFirstServerNode] Pre-downloading RKE2 images to avoid Docker Hub rate limiting...")

		cmd = "sudo mkdir -p /var/lib/rancher/rke2/agent/images"
		output, err = RunCommand(cmd, ip)
		if err != nil {
			log.Printf("[setupFirstServerNode] FAILED to create images directory: %v", err)
			return fmt.Errorf("failed to create images directory: %w", err)
		}
		log.Printf("[setupFirstServerNode] Images directory created")

		log.Printf("[setupFirstServerNode] Downloading and validating RKE2 images (this may take a few minutes)...")
		cmd = buildRKE2ImagesDownloadCommand(rke2K8sVersion)
		output, err = RunCommand(cmd, ip)
		if err != nil {
			log.Printf("[setupFirstServerNode] FAILED to download/validate images: %v", err)
			return fmt.Errorf("failed to download/validate RKE2 images: %w", err)
		}
		log.Printf("[setupFirstServerNode] Images downloaded and checksum validated successfully")

		cmd = "sudo mv /tmp/rke2-images.linux-amd64.tar.zst /var/lib/rancher/rke2/agent/images/"
		output, err = RunCommand(cmd, ip)
		if err != nil {
			log.Printf("[setupFirstServerNode] FAILED to move images: %v", err)
			return fmt.Errorf("failed to move images: %w", err)
		}
		log.Printf("[setupFirstServerNode] Images pre-loaded successfully")
	} else {
		log.Printf("[setupFirstServerNode] Image pre-loading disabled, will pull from registry")
	}

	dockerUsername := strings.TrimSpace(os.Getenv("DOCKERHUB_USERNAME"))
	dockerPassword := strings.TrimSpace(os.Getenv("DOCKERHUB_PASSWORD"))

	if dockerUsername != "" && dockerPassword != "" {
		log.Printf("[setupFirstServerNode] Configuring Docker Hub authentication...")

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

	log.Printf("[setupFirstServerNode] Verifying RKE2 binary...")
	cmd = "which rke2 || ls -la /usr/local/bin/rke2 || echo 'RKE2 binary not found in expected locations'"
	output, err = RunCommand(cmd, ip)
	log.Printf("[setupFirstServerNode] RKE2 binary check: %s", output)

	log.Printf("[setupFirstServerNode] Enabling RKE2 server service...")
	cmd = "sudo systemctl enable rke2-server.service"
	output, err = RunCommand(cmd, ip)
	if err != nil {
		log.Printf("[setupFirstServerNode] FAILED to enable RKE2 server: %v", err)
		return fmt.Errorf("failed to enable RKE2 server: %w", err)
	}
	log.Printf("[setupFirstServerNode] RKE2 server enabled. Output: %s", output)

	log.Printf("[setupFirstServerNode] Starting RKE2 server service...")
	cmd = "sudo systemctl start rke2-server.service"
	output, err = RunCommand(cmd, ip)
	if err != nil {
		log.Printf("[setupFirstServerNode] FAILED to start RKE2 server: %v", err)
		log.Printf("[setupFirstServerNode] Gathering diagnostic information...")

		cmd = "sudo systemctl status rke2-server.service --no-pager"
		statusOutput, statusErr := RunCommand(cmd, ip)
		if statusErr == nil {
			log.Printf("[setupFirstServerNode] Service status:\n%s", statusOutput)
		} else {
			log.Printf("[setupFirstServerNode] Could not get service status: %v", statusErr)
		}

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

	log.Printf("[setupFirstServerNode] Checking initial service status...")
	cmd = "sudo systemctl status rke2-server.service"
	output, _ = RunCommand(cmd, ip)
	log.Printf("[setupFirstServerNode] Service status:\n%s", output)

	log.Printf("[setupFirstServerNode] Waiting for RKE2 to initialize on %s (this may take several minutes)...", ip)
	maxRetries := 30
	for i := 0; i < maxRetries; i++ {
		log.Printf("[setupFirstServerNode] Attempt %d/%d: Checking for node-token file...", i+1, maxRetries)

		cmd = "sudo test -f /var/lib/rancher/rke2/server/node-token && echo 'ready' || echo 'not-ready'"
		status, err := RunCommand(cmd, ip)
		log.Printf("[setupFirstServerNode] Node-token check result: '%s'", status)

		if err == nil && strings.TrimSpace(status) == "ready" {
			log.Printf("[setupFirstServerNode] RKE2 initialized successfully on %s", ip)

			cmd = "sudo cat /var/lib/rancher/rke2/server/node-token"
			token, tokenErr := RunCommand(cmd, ip)
			if tokenErr != nil {
				log.Printf("[setupFirstServerNode] WARNING: Token file exists but cannot read it: %v", tokenErr)
			} else {
				log.Printf("[setupFirstServerNode] Token successfully read (length: %d)", len(token))
			}

			return nil
		}

		if i%3 == 0 {
			log.Printf("[setupFirstServerNode] Checking service status (attempt %d)...", i+1)
			cmd = "sudo systemctl status rke2-server.service --no-pager"
			statusOutput, _ := RunCommand(cmd, ip)
			log.Printf("[setupFirstServerNode] Service status:\n%s", statusOutput)

			log.Printf("[setupFirstServerNode] Checking recent logs...")
			cmd = "sudo journalctl -u rke2-server.service --no-pager -n 20"
			logsOutput, _ := RunCommand(cmd, ip)
			log.Printf("[setupFirstServerNode] Recent logs:\n%s", logsOutput)
		}

		log.Printf("[setupFirstServerNode] Waiting 10 seconds before next check...")
		time.Sleep(10 * time.Second)
	}

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

	preloadImages := viper.GetBool("rke2.preload_images")

	if preloadImages {
		log.Printf("[setupAdditionalServerNode] Pre-downloading RKE2 images for %s...", ip)

		cmd = "sudo mkdir -p /var/lib/rancher/rke2/agent/images"
		_, err = RunCommand(cmd, ip)
		if err != nil {
			log.Printf("[setupAdditionalServerNode] FAILED to create images directory: %v", err)
			return fmt.Errorf("failed to create images directory: %w", err)
		}

		log.Printf("[setupAdditionalServerNode] Downloading and validating RKE2 images for %s...", ip)
		cmd = buildRKE2ImagesDownloadCommand(rke2K8sVersion)
		_, err = RunCommand(cmd, ip)
		if err != nil {
			log.Printf("[setupAdditionalServerNode] FAILED to download/validate images: %v", err)
			return fmt.Errorf("failed to download/validate RKE2 images: %w", err)
		}

		cmd = "sudo mv /tmp/rke2-images.linux-amd64.tar.zst /var/lib/rancher/rke2/agent/images/"
		_, err = RunCommand(cmd, ip)
		if err != nil {
			log.Printf("[setupAdditionalServerNode] FAILED to move images: %v", err)
			return fmt.Errorf("failed to move images: %w", err)
		}
		log.Printf("[setupAdditionalServerNode] Images pre-loaded and validated successfully for %s", ip)
	}

	dockerUsername := strings.TrimSpace(os.Getenv("DOCKERHUB_USERNAME"))
	dockerPassword := strings.TrimSpace(os.Getenv("DOCKERHUB_PASSWORD"))

	if dockerUsername != "" && dockerPassword != "" {
		log.Printf("[setupAdditionalServerNode] Configuring Docker Hub authentication for %s...", ip)

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

	log.Printf("[setupAdditionalServerNode] Installing RKE2 version %s on %s...", rke2K8sVersion, ip)
	cmd, err = buildRKE2InstallCommand("server", rke2K8sVersion, expectedInstallerSHA256)
	if err != nil {
		return fmt.Errorf("failed to build RKE2 install command: %w", err)
	}
	_, err = RunCommand(cmd, ip)
	if err != nil {
		return fmt.Errorf("failed to install RKE2: %w", err)
	}

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

	log.Printf("Waiting for RKE2 to initialize on %s (this may take several minutes)...", ip)
	maxRetries := 30
	for i := 0; i < maxRetries; i++ {
		cmd = "sudo systemctl is-active --quiet rke2-server && echo 'active' || echo 'inactive'"
		status, err := RunCommand(cmd, ip)
		if err == nil && strings.TrimSpace(status) == "active" {
			log.Printf("RKE2 initialized successfully on %s", ip)
			return nil
		}

		time.Sleep(10 * time.Second)
	}

	return fmt.Errorf("timeout waiting for RKE2 to initialize on %s", ip)
}

func getAndSaveKubeconfig(serverIP string, haDir string) error {
	rawKubeconfig, err := RunCommand("sudo cat /etc/rancher/rke2/rke2.yaml", serverIP)
	if err != nil {
		return fmt.Errorf("failed to retrieve kubeconfig: %w", err)
	}

	configIP := fmt.Sprintf("https://%s:6443", serverIP)
	modifiedKubeconfig := strings.Replace(rawKubeconfig, "https://127.0.0.1:6443", configIP, -1)

	currentDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get current directory: %w", err)
	}

	absHADir := filepath.Join(currentDir, haDir)
	if _, err := os.Stat(absHADir); os.IsNotExist(err) {
		if mkdirErr := os.MkdirAll(absHADir, os.ModePerm); mkdirErr != nil {
			return fmt.Errorf("failed to create directory %s: %w", absHADir, mkdirErr)
		}
		log.Printf("Created directory %s", absHADir)
	}

	absKubeConfigPath := filepath.Join(absHADir, "kube_config.yaml")
	err = os.WriteFile(absKubeConfigPath, []byte(modifiedKubeconfig), 0644)
	if err != nil {
		return fmt.Errorf("failed to write kubeconfig file: %w", err)
	}

	log.Printf("Kubeconfig saved to %s", absKubeConfigPath)
	return nil
}
