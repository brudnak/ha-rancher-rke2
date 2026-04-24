# Terraform State Bootstrap

This bootstrap stack creates the persistent backend used by GitHub Actions
sign-off lanes:

- S3 bucket for Terraform state
- S3 versioning
- S3 server-side encryption
- S3 public access block
- lifecycle cleanup for old noncurrent state versions
- DynamoDB lock table with point-in-time recovery

This stack should be created once and reused. Do not create and destroy the
state bucket inside each sign-off run.

## Local Usage

```bash
cd bootstrap/terraform-state
terraform init
terraform plan \
  -var 'aws_region=us-east-2' \
  -var 'state_bucket_name=your-unique-state-bucket' \
  -var 'lock_table_name=ha-rancher-rke2-terraform-locks'
terraform apply \
  -var 'aws_region=us-east-2' \
  -var 'state_bucket_name=your-unique-state-bucket' \
  -var 'lock_table_name=ha-rancher-rke2-terraform-locks'
```

## GitHub Actions Usage

Use `.github/workflows/bootstrap-terraform-state.yml`.

The workflow expects an environment secret:

| Secret | Purpose |
| --- | --- |
| `AWS_BOOTSTRAP_ROLE_ARN` | AWS role that can create/update the bootstrap bucket and lock table through GitHub OIDC. |

The workflow defaults to plan-only. Set `apply` to `true` when you are ready to
create or update the backend. After apply, the workflow writes a non-secret
`terraform-backend-env` artifact and step-summary snippet:

```bash
TF_STATE_BUCKET=...
TF_STATE_LOCK_TABLE=...
TF_STATE_REGION=...
```

Put those values in the protected `automation-smoke` environment variables.
They are resource identifiers, not credentials, but workflow logs, summaries,
and artifacts are visible to anyone with access to Actions runs for the
repository. Keep actual AWS access in OIDC roles and environment secrets.

## Backend Key Shape

Sign-off lanes should use keys like:

```text
ha-rancher-rke2/signoff/<release-line>/<rancher-version>/<run-id>/<lane>/terraform.tfstate
```

Each lane gets a unique key so cleanup, retries, and reports stay isolated.

The sign-off planner can generate these keys:

```bash
go run ../../automation/signoff-plan \
  -rancher-version v2.14.1-alpha6 \
  -previous-rancher-version v2.14.0 \
  -run-id 123456789
```

## Terratest Backend Env Vars

The HA Terratest runner switches to this backend only when all of these are set:

```bash
export TF_STATE_BUCKET="your-unique-state-bucket"
export TF_STATE_LOCK_TABLE="ha-rancher-rke2-terraform-locks"
export TF_STATE_REGION="us-east-2"
export TF_STATE_KEY="ha-rancher-rke2/signoff/v2.14/v2.14.1-alpha6/123456789/fresh-alpha/terraform.tfstate"
```

Leave them unset for local development with local Terraform state.
