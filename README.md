# RKE2 Rancher HA Bootstrapper

Deploy Rancher High Availability (HA) clusters on AWS using RKE2 with automated setup and secure configuration.

## Key Features

- **No Cert Manager required** — SSL is handled via AWS ACM
- **Secure by default** — HTTPS enabled from deployment
- **Fully automated** — Rancher installation happens automatically
- **Simple workflow:**
  1. Configure your Helm commands in `tool-config.yml`
  2. Run the test command

Rancher is installed with `--set tls=external` since ACM certificates handle TLS termination.

## Overview

This repository provides:

- Deploy 3-node RKE2 HA clusters with Terraform
- Auto-configure each node with secure ALB integration
- Use AWS ACM for certificates (no cert-manager required)
- Generate and execute custom installation scripts
- Automatically inject correct URLs into Helm commands
- Single test command deployment

## Directory Structure

Place `tool-config.yml` at the project root:

```
.
├── README.md
├── tool-config.yml
├── go.mod
├── terratest/
│   └── test.go
├── modules/
│   └── aws/
```

## Deployment

Run the following command to deploy the infrastructure:

```bash
go test -v -run TestHaSetup -timeout 60m ./terratest
```

This command will:

- Launch EC2 instances, ALBs, and Route53 DNS records
- Configure TLS with AWS ACM certificates
- Bootstrap and join all 3 nodes into RKE2 cluster
- Generate and execute Rancher installation scripts
- Automatically inject correct URLs into Helm commands

## Rancher Installation

Rancher is installed automatically during the setup process:

1. Correct URLs are injected into each Helm command
2. Install scripts are generated for each HA instance
3. Scripts are executed to install Rancher

Installation uses ALB with ACM certificates for secure HTTPS access without requiring cert-manager.

**Note:** Install scripts remain available in each `high-availability-X/` directory for manual re-execution if needed.

## Cleanup

To destroy all resources:

```bash
go test -v -run TestHACleanup -timeout 20m ./terratest
```

This will:

- Destroy all infrastructure via Terraform
- Clean up generated files and folders
- Remove all AWS resources

## Configuration

### Sample `tool-config.yml`

For available RKE2 Kubernetes versions, refer to: [RKE2 v1.32.X Release Notes](https://docs.rke2.io/release-notes/v1.32.X)

### Important Configuration Notes

- The number of Helm commands under `rancher.helm_commands` **must match** the `total_has` value
- Each Helm command will be used for a specific HA instance (first command for first instance, etc.)
- You can customize each Helm command with different parameters (bootstrap password, version, etc.)
- The `hostname` parameter in each Helm command will be automatically replaced with the correct URL
  - You can leave it blank, use a placeholder, or include your own value (it will be overridden)
- The tool will validate that the number of commands matches `total_has` and fail with an error if they don't match
- The install script is automatically executed for each HA instance during setup

```yaml
aws:
  rsa_private_key: |
    -----BEGIN RSA PRIVATE KEY-----
    -----END RSA PRIVATE KEY-----

rancher:
  helm_commands:
    - |
      helm install rancher rancher-latest/rancher \
        --namespace cattle-system \
        --set hostname=placeholder \
        --set bootstrapPassword=your-password \
        --set tls=external \
        --set global.cattle.psp.enabled=false \
        --set rancherImageTag=v2.11.0 \
        --version 2.11.0 \
        --set agentTLSMode=system-store
    - |
      helm install rancher rancher-latest/rancher \
        --namespace cattle-system \
        --set hostname=placeholder \
        --set bootstrapPassword=your-password \
        --set tls=external \
        --set global.cattle.psp.enabled=false \
        --set rancherImageTag=v2.11.0 \
        --version 2.11.0 \
        --set agentTLSMode=system-store
      
k8s:
  version: "v1.31.4+rke2r1"

total_has: 2  # Number of HA clusters to create (must match number of helm_commands)

tf_vars:
  aws_access_key: "super-secret-key"
  aws_secret_key: "super-secret-key"
  aws_prefix: "xyz" # your initials, keep it short! 
  aws_vpc: ""
  aws_subnet_a: ""
  aws_subnet_b: ""
  aws_subnet_c: ""
  aws_ami: ""
  aws_subnet_id: ""
  aws_security_group_id: ""
  aws_pem_key_name: ""
  aws_route53_fqdn: ""
```

## Output Example

Each HA setup creates a folder like:

```
high-availability-1/
├── install.sh         # Rancher installation script
├── kube_config.yaml   # RKE2 kubeconfig
```

## Contributing

Pull requests and questions are welcome.

---

_Built with Go, Terraform, and Rancher._