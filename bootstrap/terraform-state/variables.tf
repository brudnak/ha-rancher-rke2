variable "aws_region" {
  type        = string
  description = "AWS region for the Terraform state bucket and lock table."
  default     = "us-east-2"
}

variable "state_bucket_name" {
  type        = string
  description = "Globally unique S3 bucket name for Terraform state."
}

variable "lock_table_name" {
  type        = string
  description = "DynamoDB table name for Terraform state locking."
}

variable "force_destroy" {
  type        = bool
  description = "Allow Terraform to delete the state bucket even when it contains objects. Keep false unless intentionally decommissioning the backend."
  default     = false
}

variable "noncurrent_state_version_expiration_days" {
  type        = number
  description = "Days to retain old noncurrent state object versions."
  default     = 30

  validation {
    condition     = var.noncurrent_state_version_expiration_days >= 7
    error_message = "Keep noncurrent state versions for at least 7 days."
  }
}

variable "tags" {
  type        = map(string)
  description = "Additional tags to apply to bootstrap resources."
  default     = {}
}
