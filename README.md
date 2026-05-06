# RKE2 Rancher HA Bootstrapper

Deploy Rancher High Availability (HA) clusters on AWS using RKE2 with automated setup and secure configuration.

For the scheduled GitHub Actions alpha/webhook sign-off automation, see [docs/README.md](docs/README.md).

## Key Features

- **No Cert Manager required** — SSL is handled via AWS ACM
- **Secure by default** — HTTPS enabled from deployment
- **Fully automated** — Rancher installation happens automatically
- **Simple workflow:**
  1. Configure `tool-config.yml`
  2. Run the test command

Generated Rancher Helm commands set `--set tls=external` because the AWS ALB terminates public TLS with ACM and forwards to Rancher over HTTP/80. RKE2 ingress is configured with `use-forwarded-headers: "true"` so Rancher sees the original HTTPS request and avoids redirect loops.

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
│   └── ha_test.go
├── modules/
│   └── aws/
```

## Deployment

Run the following command to deploy the infrastructure:

```bash
go test -v -run '^TestHaSetup$' -timeout 60m ./terratest
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
go test -v -run '^TestHACleanup$' -timeout 30m ./terratest
```

This will:

- Destroy all infrastructure via Terraform
- Clean up generated files and folders
- Remove all AWS resources

## Local Control Panel

To open the optional local-only Rancher control panel:

```bash
go test -v -run '^TestHAControlPanel$' -timeout 0 -count=1 ./terratest
```

This starts a browser-based control panel bound to `127.0.0.1` only. It is separate from setup and cleanup, so you can open it any time after provisioning, close it when you're done, and start it again later to re-check cluster health.

`-count=1` is recommended here so `go test` does not reuse a cached prior success and immediately exit instead of starting a fresh panel.

If you prefer using the IDE run button, `TestHAControlPanel` is also available alongside `TestHaSetup` and `TestHACleanup` in [terratest/ha_test.go](terratest/ha_test.go).

## Exact Test Runs

Live infrastructure tests are guarded on purpose. They only run when the `-run`
pattern is exactly the test name, or an anchored regex for only that test. This
prevents a broad package run such as `go test ./terratest` or a generic IDE play
button from accidentally creating or destroying cloud resources.

Use these commands for the normal local lifecycle:

```bash
# Create Rancher HA infrastructure
go test -v -run '^TestHaSetup$' -timeout 60m ./terratest

# Wait until Rancher and rancher-webhook are healthy
go test -v -run '^TestHAWaitReady$' -timeout 35m ./terratest

# Open the local control panel
go test -v -run '^TestHAControlPanel$' -timeout 0 -count=1 ./terratest

# Destroy AWS infrastructure
go test -v -run '^TestHACleanup$' -timeout 30m ./terratest
```

For GoLand, the gutter run button works for the guarded lifecycle tests as long as it generates an exact test pattern. If you create or edit a Go Test run configuration, set:

- **Test kind / Run kind:** `Package`, `Directory`, or `Pattern` is fine if the **Pattern** is exact.
- **Package path:** `github.com/brudnak/ha-rancher-rke2/terratest`
- **Pattern:** one exact pattern, for example `^TestHaSetup$`, `^TestHACleanup$`, or `^TestHAControlPanel$`
- **Go tool arguments / Additional go test arguments:** add `-timeout 30m` for cleanup, or `-timeout 0 -count=1` for the control panel

If GoLand shows `Test ignored` with a message like `uses live infrastructure; run it explicitly`, the run configuration is using a broader pattern. Change the pattern to one exact test name.

The control panel currently provides:

- Per-HA Rancher cards with URL, kubeconfig path, and reachability
- `cattle-system` visibility focused on Rancher and Rancher webhook pods
- Recent pod logs and live log streaming
- Active Rancher leader detection with a badge and change highlighting
- A guarded cleanup button that requires typing `cleanup`

The cleanup button calls the existing canonical cleanup flow (`TestHACleanup`) rather than introducing a separate destroy path.

## Configuration

Use one of these checked-in examples as your starting point:

- [tool-config.auto.example.yml](tool-config.auto.example.yml)
- [tool-config.manual.example.yml](tool-config.manual.example.yml)

Then copy the one you want to `tool-config.yml` and adjust the non-secret values.

### Environment Secrets

These four secrets are now read from environment variables only:

- `AWS_ACCESS_KEY_ID`
- `AWS_SECRET_ACCESS_KEY`
- `DOCKERHUB_USERNAME`
- `DOCKERHUB_PASSWORD`

The cleanest setup on your machine is to put them in `~/.zprofile`:

```bash
export AWS_ACCESS_KEY_ID="your-aws-access-key"
export AWS_SECRET_ACCESS_KEY="your-aws-secret-key"
export DOCKERHUB_USERNAME="your-dockerhub-username"
export DOCKERHUB_PASSWORD="your-dockerhub-password"
```

Then reload your shell:

```bash
source ~/.zprofile
```

If you do not want Docker Hub authentication, leave both Docker Hub environment variables unset.

### Sample `tool-config.yml`

For available RKE2 Kubernetes versions, refer to the [RKE2 release notes](https://docs.rke2.io/release-notes).

### Important Configuration Notes

- `rancher.mode` supports:
  - `manual` to provide full Helm commands yourself
  - `auto` to provide one or more Rancher versions and let the tool resolve chart source, image source, RKE2 version, and installer checksum for you
- In `manual` mode, the number of Helm commands under `rancher.helm_commands` **must match** `total_has`
- In `auto` mode:
  - use `rancher.version` for a single HA
  - use `rancher.versions` for multiple HAs, with exactly one version per HA
- Each Helm command will be used for a specific HA instance (first command for first instance, etc.)
- You can customize each Helm command with different parameters (bootstrap password, version, etc.)
- The `hostname` parameter in each Helm command will be automatically replaced with the correct URL
  - You can leave it blank, use a placeholder, or include your own value (it will be overridden)
- The tool validates your config shape and fails early if the number of versions or Helm commands does not match `total_has`
- The install script is automatically executed for each HA instance during setup
- In `manual` mode:
  - use `k8s.version` for a single HA
  - use `k8s.versions` for multiple HAs, with exactly one RKE2 version per HA
  - use `rke2.install_script_sha256` for a single HA
  - use `rke2.install_script_sha256s` for multiple HAs, keyed by exact RKE2 version
- `rke2.preload_images: true` downloads the RKE2 image bundle before install to help avoid Docker Hub rate limits
- `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` must be set in your shell environment
- `DOCKERHUB_USERNAME` and `DOCKERHUB_PASSWORD` are optional environment variables
  - If you set them, the tool creates `/etc/rancher/rke2/registries.yaml` so RKE2 can authenticate to Docker Hub
  - If you leave them unset, the tool skips Docker Hub authentication
- In `auto` mode, the tool prints a resolved plan for each HA and asks you to continue before provisioning starts
- RKE2 installer and image downloads are checksum-validated before use; the install path does not use `curl | bash`

### Supply chain security

RKE2 artifacts downloaded onto cluster nodes are checksum-verified before they
are used. This protects against a compromised upstream release asset, or a
tampered download in transit, reaching the node bootstrap path.

| Pattern | Location | Status |
|---|---|---|
| `curl \| bash` for RKE2 install | — | Eliminated — the installer is downloaded to a temp file, verified, then executed |
| RKE2 installer download | [terratest/preflight.go](terratest/preflight.go), [terratest/rancher_plan.go](terratest/rancher_plan.go) | Hardened — SHA256 is resolved or required before provisioning, then verified by Go preflight |
| RKE2 remote installer execution | [terratest/preflight.go](terratest/preflight.go), [terratest/cluster_setup.go](terratest/cluster_setup.go) | Hardened — remote bash verifies the same SHA256 before running `install.sh` |
| RKE2 image tarball preload | [terratest/preflight.go](terratest/preflight.go), [terratest/cluster_setup.go](terratest/cluster_setup.go) | Hardened — `rke2-images.linux-amd64.tar.zst` is validated with the official release checksum file before it is moved into place |

**RKE2 installer script**

The installer is validated twice — once in Go before provisioning starts, and once in bash on the remote node:

1. Before provisioning, the Go preflight downloads `install.sh` for the pinned RKE2 version and computes its SHA256 using `crypto/sha256`. If the hash does not match the expected value, provisioning is blocked entirely.
2. On each node, the install command downloads the script to a temp file, runs `sha256sum -c` against the same hash, and refuses to execute if validation fails — with a clear `SECURITY ERROR` message in stderr.

Where the expected hash comes from depends on mode:

- **`manual` mode** — you supply `rke2.install_script_sha256` (single HA) or `rke2.install_script_sha256s` (multiple HAs) explicitly in your config. The tool checks your pinned value before any node is touched.
- **`auto` mode** — the tool fetches the versioned `install.sh` while resolving the plan, computes its SHA256, and stores it in the resolved plan. That computed hash is then used for both the Go preflight check and the per-node bash validation, so the same two-step process applies in both modes.

**RKE2 images tarball**

When `rke2.preload_images: true` is set, the image bundle is also validated before it is moved into place. This applies equally in `manual` and `auto` modes — in both cases the RKE2 version is known before the download starts (from your config in manual mode, from the resolved plan in auto mode), so the same validation runs regardless:

1. The tarball (`rke2-images.linux-amd64.tar.zst`) is downloaded to `/tmp`.
2. The official `sha256sum-amd64.txt` for that exact RKE2 release is downloaded from the same GitHub release page.
3. `sha256sum -c` is run against the matching entry in that checksum file.
4. If validation fails the tarball and checksum file are deleted and the script exits with a `SECURITY ERROR` — the corrupted file never reaches `/var/lib/rancher/rke2/agent/images/`.

### Auto Mode Example

Use `auto` mode when you want to provide a Rancher version and let the tool resolve the rest.

```yaml
rancher:
  mode: auto
  versions:
    - "2.13-head"
    - "2.13.4"
  distro: auto
  bootstrap_password: "your-password"
  auto_approve: false

rke2:
  preload_images: true

total_has: 2  # Number of HA clusters to create (must match number of rancher.versions in auto mode)

tf_vars:
  aws_region: "us-east-2"
  aws_prefix: "xyz" # 2 or 3 letters, usually your initials
  aws_vpc: ""
  aws_subnet_a: ""
  aws_subnet_b: ""
  aws_subnet_c: ""
  aws_ami: ""
  aws_subnet_id: ""
  aws_security_group_id: ""
  aws_pem_key_name: ""
  aws_route53_fqdn: ""
  # Optional custom Rancher DNS label, for example "brudnak" -> brudnak.<aws_route53_fqdn>.
  # Requires total_has: 1. Omit or leave blank for generated names.
  custom_hostname_prefix: ""
```

In `auto` mode, the tool will:

1. Resolve the Rancher chart repo and chart version for each HA version you requested
2. Resolve the Rancher image settings for each HA
3. Look up a supported RKE2 minor from the Rancher support matrix
4. Pick the latest patch release in that RKE2 line
5. Resolve the installer SHA256 for that exact RKE2 version
6. Generate one Helm command per HA and inject the correct URL later during setup
7. Print the generated plan(s)
8. Ask you to continue or cancel before provisioning

For a single HA, you can use this shorter config:

```yaml
rancher:
  mode: auto
  version: "2.13-head"
  distro: auto
  bootstrap_password: "your-password"
  auto_approve: false

total_has: 1
```

If you do not want Docker Hub authentication, leave both `DOCKERHUB_USERNAME` and `DOCKERHUB_PASSWORD` unset in your shell.

### Manual Mode Example

Use `manual` mode when you want full control over the Helm commands.

```yaml
rancher:
  mode: manual
  helm_commands:
    - |
      helm install rancher rancher-prime/rancher \
        --namespace cattle-system \
        --version 2.13.4 \
        --set hostname=placeholder \
        --set bootstrapPassword=your-password \
        --set tls=external \
        --set global.cattle.psp.enabled=false \
        --set rancherImage=registry.rancher.com/rancher/rancher \
        --set rancherImageTag=v2.13.4 \
        --set agentTLSMode=system-store
    - |
      helm install rancher rancher-latest/rancher \
        --namespace cattle-system \
        --version 2.14.0 \
        --set hostname=placeholder \
        --set bootstrapPassword=your-password \
        --set tls=external \
        --set global.cattle.psp.enabled=false \
        --set rancherImageTag=v2.14.0 \
        --set agentTLSMode=system-store

total_has: 2

k8s:
  versions:
    - "v1.33.7+rke2r1"
    - "v1.34.6+rke2r1"

rke2:
  install_script_sha256s:
    v1.33.7+rke2r1: "bfbd978d603b7070f5748c934326db509bf1470c97d3f61a3aaa6e2eed6bd054"
    v1.34.6+rke2r1: "2d24db2184dd6b1a5e281fa45cc9a8234c889394721746f89b5fe953fdaaf40a"
  preload_images: true
```

For a single manual HA, the older shorter form still works:

```yaml
k8s:
  version: "v1.33.7+rke2r1"

rke2:
  install_script_sha256: "bfbd978d603b7070f5748c934326db509bf1470c97d3f61a3aaa6e2eed6bd054"
```

### Updating the RKE2 checksum

You only need to update the checksum values manually when you use `manual` mode and change the matching RKE2 version.

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

## Cleanup Cost Estimate

`TestHACleanup` now prints a best-effort AWS cost estimate after destroy for:

- EC2 runtime
- EBS root volumes

This is only an estimate, not an AWS bill.

The estimate uses:

- live AWS pricing data for EC2 and EBS unit prices
- actual EC2 instance launch times from AWS to estimate runtime
- actual attached root EBS volumes from AWS to estimate storage cost

It does **not** include everything AWS might charge for, such as:

- ALB usage
- Route53 charges
- data transfer
- request-driven costs

So the number is meant to be helpful and roughly right for the main infrastructure cost drivers, not a final billing total.

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
