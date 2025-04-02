# 🚀 RKE2 Rancher HA Bootstrapper

Welcome to the easiest way to spin up **Rancher High Availability (HA)** clusters on AWS using RKE2! 🐄  
This repo automates everything from Terraform infra to Rancher install — just tweak the config and go! 🚀🌩️

---

## 🧠 What This Repo Does

This tool helps you:

- 🌐 Provision a **3-node RKE2 HA cluster** per instance with Terraform  
- 🔧 Auto-configure each node and load balancer  
- 🐮 Generate a ready-to-run Rancher `install.sh` script  
- 🎯 Simplify setup to a single test command  

Perfect for testing Rancher HA setups or building real environments!

---

## 🛠️ How It Works

- Run the Go test suite 🧪  
- Terraform provisions 3 EC2 nodes per HA set  
- Nodes get configured with RKE2 and joined into a cluster  
- ALB + DNS + TLS = ✔️  
- Rancher install script is created and saved locally  
- You run `install.sh` to install Rancher on your new cluster!

---

## 🧪 Usage

### 1️⃣ Create a `tool-config.yml` file

⚠️ Place it at the **project root**, right next to `README.md`.

📁 Your directory structure should look like:

```
.
├── README.md
├── tool-config.yml  ✅
├── go.mod
├── terratest/
│   └── test.go
├── modules/
│   └── aws/
```

➡️ See below for a complete sample `tool-config.yml`.

---

### 2️⃣ Run the test to build your HA clusters:

```bash
go test -v -timeout 60m
```

This will:
- 🌍 Launch EC2s, ALBs, Route53 records via Terraform
- 🧠 Auto-configure RKE2 across all nodes
- ✍️ Create install scripts and kubeconfigs for each cluster

---

### 3️⃣ Navigate to a generated HA folder and run:

```bash
./install.sh
```

Boom 💥 — Rancher is up and running! 🐄

---

## 🧾 Sample `tool-config.yml`

```yaml
aws:
  rsa_private_key: |
    -----BEGIN RSA PRIVATE KEY-----
    -----END RSA PRIVATE KEY-----

rancher:
  bootstrap_password: ""
  image_tag: "v2.11.0"
  version: "2.11.0"

k8s:
  version: "v1.31.4+rke2r1"

total_has: 2  # Number of HA clusters to create

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

For each HA cluster, a folder like this is created:

```
high-availability-1/
├── install.sh           # Rancher install script
├── kube_config.yaml     # RKE2 kubeconfig
```

Use these to install Rancher and access your cluster!

---

## 🧼 Cleanup

When you're done, clean up all resources with:

```bash
go test -run TestHACleanup
```

This will:
- 💣 Destroy the infra
- 🧹 Clean up temp files and directories

---

## 💬 Final Notes

This tool was made to make Rancher HA fun and painless.  
Tweak the install script, adjust the Terraform as needed, and deploy away! 🐮🌎

Pull requests welcome. Happy Ranching! 🧑‍🌾🌾

---

_🌟 Built with Go, Terraform, and Rancher love._
