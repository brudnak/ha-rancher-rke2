package test

import (
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
)

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
		"../modules/aws/backend.tf",
		"../modules/aws/terraform.tfstate",
		"../modules/aws/terraform.tfstate.backup",
		"../modules/aws/terraform.tfvars",
	}

	for _, file := range files {
		RemoveFile(file)
	}

	RemoveFolder("../modules/aws/.terraform")
}

func automationOutputDir() string {
	if workspace := strings.TrimSpace(os.Getenv("GITHUB_WORKSPACE")); workspace != "" {
		return filepath.Join(workspace, "automation-output")
	}
	return "automation-output"
}

func automationOutputPath(name string) string {
	return filepath.Join(automationOutputDir(), name)
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

	currentDir, err := os.Getwd()
	if err != nil {
		log.Printf("Failed to get current directory: %v", err)
		return
	}

	absHADir := filepath.Join(currentDir, haDir)
	if _, err := os.Stat(absHADir); os.IsNotExist(err) {
		if mkdirErr := os.MkdirAll(absHADir, os.ModePerm); mkdirErr != nil {
			log.Printf("Failed to create directory %s: %v", absHADir, mkdirErr)
			return
		}
		log.Printf("Created directory %s", absHADir)
	}

	absInstallScriptPath := filepath.Join(absHADir, "install.sh")
	writeFile(absInstallScriptPath, []byte(installScript))

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
