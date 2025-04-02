# modules/rke2-ha/main.tf

# Variables
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

# Resources
resource "random_pet" "name" {
  keepers = {
    aws_prefix = var.aws_prefix
  }
  length    = 1
  separator = "-"
}

resource "random_id" "unique" {
  byte_length = 2
  keepers = {
    aws_prefix = var.aws_prefix
  }
}

locals {
  name_prefix = "${var.aws_prefix}-${random_pet.name.id}-${random_id.unique.hex}"
  domain_name = "${local.name_prefix}.${var.aws_route53_fqdn}"
}

resource "aws_instance" "aws_instance" {
  count                  = 3
  ami                    = var.aws_ami
  instance_type          = "t3a.large"
  subnet_id              = var.aws_subnet_id
  vpc_security_group_ids = [var.aws_security_group_id]
  key_name               = var.aws_pem_key_name

  root_block_device {
    volume_size = 150
  }

  tags = {
    Name = "${local.name_prefix}-${count.index + 1}"
  }
}

# Application Load Balancer for Rancher UI (80/443)
resource "aws_lb_target_group" "aws_lb_target_group_80" {
  name        = "${local.name_prefix}-80"
  port        = 80
  protocol    = "HTTP"
  target_type = "instance"
  vpc_id      = var.aws_vpc
  health_check {
    protocol          = "HTTP"
    port              = "traffic-port"
    healthy_threshold = 3
    interval          = 10
  }
}

resource "aws_lb_target_group" "aws_lb_target_group_443" {
  name        = "${local.name_prefix}-443"
  port        = 443
  protocol    = "HTTPS"
  target_type = "instance"
  vpc_id      = var.aws_vpc
  health_check {
    protocol          = "HTTPS"
    port              = 443
    healthy_threshold = 3
    interval          = 10
  }
}

resource "aws_lb_target_group_attachment" "attach_tg_80" {
  count            = length(aws_instance.aws_instance)
  target_group_arn = aws_lb_target_group.aws_lb_target_group_80.arn
  target_id        = aws_instance.aws_instance[count.index].id
  port             = 80
}

resource "aws_lb_target_group_attachment" "attach_tg_443" {
  count            = length(aws_instance.aws_instance)
  target_group_arn = aws_lb_target_group.aws_lb_target_group_443.arn
  target_id        = aws_instance.aws_instance[count.index].id
  port             = 443
}

resource "aws_lb" "aws_lb" {
  load_balancer_type = "application"
  name               = local.name_prefix
  internal           = false
  ip_address_type    = "ipv4"
  subnets            = [var.aws_subnet_a, var.aws_subnet_b, var.aws_subnet_c]
}

resource "aws_lb_listener" "aws_lb_listener_80" {
  load_balancer_arn = aws_lb.aws_lb.arn
  port              = "80"
  protocol          = "HTTP"

  default_action {
    type = "redirect"
    redirect {
      port        = "443"
      protocol    = "HTTPS"
      status_code = "HTTP_301"
    }
  }
}

# Route53 and ACM Certificate configuration
data "aws_route53_zone" "zone" {
  name = var.aws_route53_fqdn
}

resource "aws_route53_record" "aws_route53_record" {
  zone_id = data.aws_route53_zone.zone.zone_id
  name    = local.name_prefix
  type    = "CNAME"
  ttl     = "60"
  records = [aws_lb.aws_lb.dns_name]
}

resource "aws_acm_certificate" "cert" {
  domain_name       = local.domain_name
  validation_method = "DNS"

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_route53_record" "cert_validation" {
  count = 1

  name    = tolist(aws_acm_certificate.cert.domain_validation_options)[count.index].resource_record_name
  type    = tolist(aws_acm_certificate.cert.domain_validation_options)[count.index].resource_record_type
  zone_id = data.aws_route53_zone.zone.zone_id
  records = [tolist(aws_acm_certificate.cert.domain_validation_options)[count.index].resource_record_value]
  ttl     = 60
}

resource "aws_acm_certificate_validation" "cert" {
  certificate_arn         = aws_acm_certificate.cert.arn
  validation_record_fqdns = aws_route53_record.cert_validation[*].fqdn
}

resource "aws_lb_listener" "aws_lb_listener_443" {
  load_balancer_arn = aws_lb.aws_lb.arn
  port              = "443"
  protocol          = "HTTPS"
  ssl_policy        = "ELBSecurityPolicy-2016-08"
  certificate_arn   = aws_acm_certificate_validation.cert.certificate_arn

  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.aws_lb_target_group_443.arn
  }
}

# Outputs
output "server1_ip" {
  value = aws_instance.aws_instance[0].public_ip
}

output "server2_ip" {
  value = aws_instance.aws_instance[1].public_ip
}

output "server3_ip" {
  value = aws_instance.aws_instance[2].public_ip
}

output "server1_private_ip" {
  value = aws_instance.aws_instance[0].private_ip
}

output "server2_private_ip" {
  value = aws_instance.aws_instance[1].private_ip
}

output "server3_private_ip" {
  value = aws_instance.aws_instance[2].private_ip
}

output "aws_lb" {
  value = aws_lb.aws_lb.dns_name
}

output "rancher_url" {
  value = local.domain_name
}
