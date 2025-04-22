# 🐮✨ RKE2 Rancher HA Bootstrapper ✨🐮

Welcome to the **easiest**, **chillest**, and most 🔥 way to spin up **Rancher High Availability (HA)** clusters on AWS using RKE2!  
Just vibe, tweak a config, run a test, and you're Rancher-ready. 🌈⚡️🚀

---

## 💡 TL;DR – Why This Rocks

✅ **No Cert Manager needed** — SSL is done via **AWS ACM** 🙌  
✅ **Secure by default** — HTTPS from the jump 🔐  
✅ **Fully automated** — Rancher installation happens automatically 🤖  
✅ **All you gotta do:**  
1. 🛠️ Configure your Helm commands in `tool-config.yml`  
2. 🚀 Run the test — donezo!

We install Rancher using:

```bash
--set tls=external
```

Because ACM certs are **already there**, TLS is **handled**. No drama. Just Rancher 🐮💕

---

## 🧠 What This Repo Does

This repo helps you:

- 🌍 Deploy **3-node RKE2 HA clusters** with Terraform
- 🧠 Auto-configure each node & wire them up over a secure ALB
- 🔒 Use AWS ACM for certs — no cert-manager required!
- ✍️ Generate and execute a custom `install.sh` script to install Rancher in 1 command
- 🔄 Automatically inject the correct URL into each Helm command
- 🎯 All driven by a single test function, because... we love automation

---

## 📦 Directory Layout

Put your `tool-config.yml` next to this README — right at the **project root**:

```
.
├── README.md
├── tool-config.yml  🧙‍♂️ (put it here)
├── go.mod
├── terratest/
│   └── test.go
├── modules/
│   └── aws/
```

---

## 🧪 Spin It Up (HA Setup)

Run this to build everything (with timeout so it doesn’t hang forever):

```bash
go test -v -run TestHaSetup -timeout 60m ./terratest
```

🎉 This will:

- 🚀 Launch EC2s, ALBs, and Route53 DNS records
- 🔐 Setup TLS with AWS ACM certs
- 🧠 Bootstrap and join all 3 nodes into RKE2
- 📝 Generate and execute a Rancher `install.sh` script in each HA folder
- 🔄 Automatically inject the correct URL into each Helm command

---

## 🐮 Rancher Installation (Automatic)

Rancher is now installed automatically during the setup process! The tool:

1. 🔄 Injects the correct URL into each Helm command
2. 📝 Generates the install script for each HA instance
3. 🚀 Executes the script to install Rancher

This installs Rancher securely via ALB + ACM certs with TLS 🔒  
No cert-manager needed. No cluster pain. Just good vibes and cattle ✨🐄

> 💡 **Note:** The install scripts are still available in each `high-availability-X/` directory if you need to run them again or modify them.

---

## 💣 Tear It Down (Cleanup)

When you're done, run cleanup:

```bash
go test -v -run TestHACleanup -timeout 20m ./terratest
```

💥 This will:

- 💨 Destroy all infra via Terraform
- 🧹 Clean up generated files and folders
- 🧼 Leave your AWS nice and tidy

---

## 🧾 Sample `tool-config.yml`

🔎 Where to find available rke2 k8s versions:

[👨‍🌾🧙‍RKE2 v1.32.X Release Notes 👨‍🌾🧙‍♂️](https://docs.rke2.io/release-notes/v1.32.X)

### 🚨 Important Configuration Notes

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

---

## 📁 Output Example

Each HA setup creates a folder like:

```
high-availability-1/
├── install.sh         🐚 One-command Rancher installer
├── kube_config.yaml   📄 Your RKE2 kubeconfig
```

You're basically a Rancher wizard now 🧙‍♀️✨

---

## 🧡 Final Notes

This tool was built to make Rancher HA setup fun, secure, and dead simple.  
With Terraform, RKE2, and ACM doing the heavy lifting — you just ride the Rancher wave 🌊🐄

---

**Pull requests welcome. Questions welcome. Rancher users always welcome.**  
Happy HA'ing! 🌟🐮💫

---

_🌟 Built with Go, Terraform, and Rancher love._
