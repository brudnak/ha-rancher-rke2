# IAM role for SSM access
resource "aws_iam_role" "ssm_role" {
  name = "${var.aws_prefix}-ssm-role"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Action = "sts:AssumeRole"
      Effect = "Allow"
      Principal = {
        Service = "ec2.amazonaws.com"
      }
    }]
  })

  tags = {
    Name = "${var.aws_prefix}-ssm-role"
  }
}

# Attach AWS managed SSM policy
resource "aws_iam_role_policy_attachment" "ssm_policy" {
  role       = aws_iam_role.ssm_role.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

# Create instance profile
resource "aws_iam_instance_profile" "ssm_profile" {
  name = "${var.aws_prefix}-ssm-profile"
  role = aws_iam_role.ssm_role.name

  tags = {
    Name = "${var.aws_prefix}-ssm-profile"
  }
}