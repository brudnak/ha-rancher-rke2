# main.tf in root directory
terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

provider "aws" {
  region     = var.aws_region
  access_key = var.aws_access_key
  secret_key = var.aws_secret_key
}

# Variables
variable "total_has" {
  type        = number
  description = "Number of HA instances to create"
  default     = 1
}

variable "aws_region" {
  type        = string
  description = "AWS region"
  default     = "us-east-2"
}

variable "aws_access_key" {
  type        = string
  description = "AWS access key"
}

variable "aws_secret_key" {
  type        = string
  description = "AWS secret key"
}

variable "aws_prefix" {
  type        = string
  description = "Prefix for resource names"
}

variable "aws_vpc" {
  type        = string
  description = "VPC ID"
}

variable "aws_subnet_a" {
  type        = string
  description = "Subnet A ID"
}

variable "aws_subnet_b" {
  type        = string
  description = "Subnet B ID"
}

variable "aws_subnet_c" {
  type        = string
  description = "Subnet C ID"
}

variable "aws_ami" {
  type        = string
  description = "AMI ID for instances"
}

variable "aws_subnet_id" {
  type        = string
  description = "Subnet ID for instances"
}

variable "aws_security_group_id" {
  type        = string
  description = "Security group ID"
}

variable "aws_pem_key_name" {
  type        = string
  description = "Name of the PEM key for SSH access"
}

variable "aws_route53_fqdn" {
  type        = string
  description = "Route53 FQDN for DNS records"
}

# Module configuration
locals {
  ha_instances = { for i in range(1, var.total_has + 1) : i => "${var.aws_prefix}-${i}" }
}

module "ha" {
  for_each = local.ha_instances
  source   = "./modules/rke2-ha"

  aws_prefix           = each.value
  aws_vpc              = var.aws_vpc
  aws_subnet_a         = var.aws_subnet_a
  aws_subnet_b         = var.aws_subnet_b
  aws_subnet_c         = var.aws_subnet_c
  aws_ami              = var.aws_ami
  aws_subnet_id        = var.aws_subnet_id
  aws_security_group_id = var.aws_security_group_id
  aws_pem_key_name     = var.aws_pem_key_name
  aws_route53_fqdn     = var.aws_route53_fqdn
}

# Outputs
output "ha_details" {
  value = {
    for idx, instance in module.ha : "ha_${idx}" => {
      server1_ip = instance.server1_ip
      server2_ip = instance.server2_ip
      server3_ip = instance.server3_ip
      server1_private_ip = instance.server1_private_ip
      server2_private_ip = instance.server2_private_ip
      server3_private_ip = instance.server3_private_ip
      aws_lb = instance.aws_lb
      rancher_url = instance.rancher_url
    }
  }
}

output "flat_outputs" {
  value = merge([
    for idx, instance in module.ha : {
      "ha_${idx}_server1_ip"         = instance.server1_ip
      "ha_${idx}_server2_ip"         = instance.server2_ip
      "ha_${idx}_server3_ip"         = instance.server3_ip
      "ha_${idx}_server1_private_ip" = instance.server1_private_ip
      "ha_${idx}_server2_private_ip" = instance.server2_private_ip
      "ha_${idx}_server3_private_ip" = instance.server3_private_ip
      "ha_${idx}_aws_lb"             = instance.aws_lb
      "ha_${idx}_rancher_url"        = instance.rancher_url
    }
  ]...)
}