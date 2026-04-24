output "state_bucket_name" {
  description = "S3 bucket name for Terraform state."
  value       = aws_s3_bucket.state.bucket
}

output "state_bucket_arn" {
  description = "S3 bucket ARN for Terraform state."
  value       = aws_s3_bucket.state.arn
}

output "lock_table_name" {
  description = "DynamoDB lock table name."
  value       = aws_dynamodb_table.locks.name
}

output "lock_table_arn" {
  description = "DynamoDB lock table ARN."
  value       = aws_dynamodb_table.locks.arn
}

output "backend_config_example" {
  description = "Example backend config values for sign-off lanes."
  value = {
    bucket         = aws_s3_bucket.state.bucket
    dynamodb_table = aws_dynamodb_table.locks.name
    region         = var.aws_region
    encrypt        = true
  }
}

output "state_access_policy_json" {
  description = "IAM policy JSON for Terraform state bucket and lock table access."
  value       = data.aws_iam_policy_document.state_access.json
}
