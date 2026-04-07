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
- `k8s.version` is the RKE2 version to install
- `rke2.install_script_sha256` is the SHA256 of the exact RKE2 installer script for that version
- `rke2.preload_images: true` downloads the RKE2 image bundle before install to help avoid Docker Hub rate limits
- `dockerhub.username` and `dockerhub.password` are optional
  - If you set them, the tool creates `/etc/rancher/rke2/registries.yaml` so RKE2 can authenticate to Docker Hub
  - If you leave them blank, the tool skips Docker Hub authentication
- The project does not use `curl | sh` for the RKE2 installer anymore
  - It downloads the versioned installer script
  - It checks that script against the pinned SHA256
  - It only runs the script if the checksum matches

```yaml
rancher:
  helm_commands:
    - |
      helm install rancher rancher-latest/rancher \
        --namespace cattle-system \
        --set hostname=placeholder \
        --set bootstrapPassword=your-password \
        --set tls=external \
        --set global.cattle.psp.enabled=false \
        --set rancherImageTag=v2.14.0 \
        --version 2.14.0 \
        --set agentTLSMode=system-store
    - |
      helm install rancher rancher-latest/rancher \
        --namespace cattle-system \
        --set hostname=placeholder \
        --set bootstrapPassword=your-password \
        --set tls=external \
        --set global.cattle.psp.enabled=false \
        --set rancherImageTag=v2.14.0 \
        --version 2.14.0 \
        --set agentTLSMode=system-store
      
k8s:
  version: "v1.33.7+rke2r1"

rke2:
  install_script_sha256: "bfbd978d603b7070f5748c934326db509bf1470c97d3f61a3aaa6e2eed6bd054"
  preload_images: true

total_has: 2  # Number of HA clusters to create (must match number of helm_commands)

tf_vars:
  aws_region: "us-east-2"
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

dockerhub:
  username: ""
  password: ""
```

If you do not want Docker Hub authentication, leave both `dockerhub.username` and `dockerhub.password` blank.

### Updating the RKE2 checksum

You only need to update `rke2.install_script_sha256` when you change `k8s.version`.

1. Pick the RKE2 version you want.
2. Download that exact installer script.
3. Compute its SHA256.
4. Paste the hash into `tool-config.yml`.

Run:

```bash
export RKE2_VERSION="v1.33.7+rke2r1"
curl -fsSL "https://raw.githubusercontent.com/rancher/rke2/${RKE2_VERSION}/install.sh" -o /tmp/rke2-install.sh
shasum -a 256 /tmp/rke2-install.sh
```

You will get output like:

```text
bfbd978d603b7070f5748c934326db509bf1470c97d3f61a3aaa6e2eed6bd054  /tmp/rke2-install.sh
```

Copy only the hash on the left and put it into `tool-config.yml`:

```yaml
k8s:
  version: "v1.33.7+rke2r1"

rke2:
  install_script_sha256: "bfbd978d603b7070f5748c934326db509bf1470c97d3f61a3aaa6e2eed6bd054"
  preload_images: true
```

If the downloaded installer does not match the pinned hash, the setup stops immediately and refuses to run it.

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
