# GitHub Actions Setup

This repo can run disposable Rancher alpha smoke/sign-off lanes in GitHub
Actions while still keeping local `tool-config.yml` usage unchanged.

The GitHub Actions path is intentionally environment-gated:

- `automation-bootstrap` creates or updates the Terraform state backend.
- `automation-smoke` provisions Rancher, optional Linode downstreams, optional
  direct `rancher/tests` suite runs, reports results, and cleans up.

Do not put cloud credentials, Rancher tokens, kubeconfigs, or generated `.env`
files in GitHub variables, logs, reports, or artifacts.

## Repository Environments

Create these GitHub environments under repository settings.

| Environment | Purpose | Recommended protection |
| --- | --- | --- |
| `automation-bootstrap` | One-time S3/DynamoDB state backend bootstrap. | Required reviewers. |
| `automation-smoke` | Live AWS/Linode Rancher smoke lanes. | Required reviewers. |

## Bootstrap Environment

`automation-bootstrap` needs one secret:

| Secret | Required | Purpose |
| --- | --- | --- |
| `AWS_BOOTSTRAP_ROLE_ARN` | yes | AWS OIDC role that can create/update the S3 state bucket and DynamoDB lock table. |

Run `.github/workflows/bootstrap-terraform-state.yml` first with `apply=false`.
After reviewing the plan, run it again with `apply=true`.

The apply run writes a non-secret `terraform-backend-env` artifact and step
summary:

```bash
TF_STATE_BUCKET=...
TF_STATE_LOCK_TABLE=...
TF_STATE_REGION=...
```

Copy those values into `automation-smoke` environment variables. Bucket and
table names are not credentials, but they are visible to anyone who can view
workflow logs/artifacts for this repository.

## Smoke Environment Secrets

`automation-smoke` secrets:

| Secret | Required | Purpose |
| --- | --- | --- |
| `AWS_AUTOMATION_ROLE_ARN` | yes | AWS OIDC role used by live smoke lanes. |
| `RANCHER_BOOTSTRAP_PASSWORD` | yes | Initial Rancher admin password rendered into generated `tool-config.yml`. |
| `LINODE_TOKEN` | yes for downstream lanes | Linode token used by Rancher to create the disposable downstream K3s node. |
| `DOCKERHUB_USERNAME` | optional | Docker Hub auth for RKE2 pulls when needed. |
| `DOCKERHUB_PASSWORD` | optional | Docker Hub auth for RKE2 pulls when needed. |
The workflow masks repository secrets, generated Rancher admin tokens, and the
generated Linode root password before noisy provisioning steps.

## Smoke Environment Variables

`automation-smoke` variables:

| Variable | Required | Purpose |
| --- | --- | --- |
| `TF_STATE_BUCKET` | yes | S3 bucket from bootstrap output. |
| `TF_STATE_LOCK_TABLE` | yes | DynamoDB lock table from bootstrap output. |
| `TF_STATE_REGION` | yes | AWS region for the Terraform backend. |
| `AWS_REGION` | yes | AWS region for Rancher infrastructure. |
| `AWS_VPC` | yes | Existing VPC ID. |
| `AWS_SUBNET_A` | yes | Existing subnet for HA node/security wiring. |
| `AWS_SUBNET_B` | yes | Existing subnet for HA node/security wiring. |
| `AWS_SUBNET_C` | yes | Existing subnet for HA node/security wiring. |
| `AWS_AMI` | yes | AMI used by Rancher HA nodes. |
| `AWS_SUBNET_ID` | yes | Subnet ID used by EC2 instances. |
| `AWS_SECURITY_GROUP_ID` | yes | Security group ID used by EC2 instances. |
| `AWS_PEM_KEY_NAME` | yes | Existing EC2 key pair name expected by the Terraform module. |
| `AWS_ROUTE53_FQDN` | yes | Route53 zone/domain suffix used for Rancher DNS records. |
| `RANCHER_TESTS_REF` | optional | Ref to clone from `https://github.com/rancher/tests.git`; defaults to `main`. |
| `RANCHER_TEST_SUITE_SETTLE_SECONDS` | optional | Pause between direct `rancher/tests` suites; defaults to `30`. |

## Workflows

| Workflow | Creates cloud resources | Notes |
| --- | --- | --- |
| `signoff-plan.yml` | no, but it can dispatch the runner | Scheduled/manual plan generation. Scheduled runs dispatch the next uncovered sign-off lane from the ledger-filtered plan. Manual runs only dispatch when `dispatch_runs=true`. |
| `bootstrap-terraform-state.yml` | yes, only when `apply=true` | Creates or updates the persistent S3/DynamoDB backend. |
| `run-alpha-webhook-signoff.yml` | yes | Runs one alpha/webhook sign-off lane, optionally with Linode downstreams and direct `rancher/tests` suite runs, then cleans up. |

## First Live Run

After environments, secrets, and variables are configured:

1. Run `Plan Alpha/Webhook Sign-Off` manually for a known alpha, for example
   `v2.13.5-alpha5`, with `dispatch_runs=false`.
2. Run `Run Alpha/Webhook Sign-Off` with:
   - `rancher_version`: `v2.13.5-alpha5`
   - `lane`: `fresh-alpha-local-suites`
   - `keep_infra_on_failure`: `false`
   - `run_rancher_tests`: `false`
3. Confirm the run provisions Rancher, waits for readiness, renders a report,
   uploads only safe artifacts, and destroys AWS infrastructure.
4. Next, run `fresh-alpha` with `run_rancher_tests=false` to prove the
   single-node Linode downstream and downstream cleanup.
5. After those are clean, enable `run_rancher_tests=true` to clone
   `https://github.com/rancher/tests.git` and run the lane's suites in the same
   workflow job. The local-suite lane runs framework regression plus VAI
   disabled for Rancher 2.11 and older and VAI enabled for Rancher 2.12 and
   newer. Downstream webhook lanes run webhook security settings for Rancher
   2.14 and newer when the actual Rancher chart should contain those settings.
6. After manual smoke is clean, leave `Plan Alpha/Webhook Sign-Off` scheduled.
   It runs hourly at minute 19 as a safety net and dispatches one uncovered lane
   at a time only when no lane runner is already queued or running. Successful
   lane runs also wake the planner through GitHub's `workflow_run` event, so the
   next lane starts as soon as the globally serialized runner is free. If the
   ledger-filtered plan has no remaining lanes, it dispatches nothing.

Use `keep_infra_on_failure=true` only for manual debugging. It can leave AWS and
Linode resources running.

## Safe Artifacts

The smoke workflow uploads:

- `signoff-plan.json`
- `lane.env`
- `automation-output/signoff-report.md`
- non-secret `automation-output/*.json`
- `test-results/*.xml` when `run_rancher_tests=true`

It does not upload:

- generated suite `.env` files
- Rancher admin tokens
- kubeconfigs
- Terraform state files
- AWS credentials
- Linode tokens

## Cleanup

Normal smoke runs clean up automatically when `keep_infra_on_failure=false`.
