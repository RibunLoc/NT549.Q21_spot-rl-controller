# Terraform + AWS provider config.
# Backend: local state (đủ cho seminar). Production nên dùng S3 + DynamoDB lock.

terraform {
  required_version = ">= 1.5.0"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

provider "aws" {
  region = var.aws_region

  default_tags {
    tags = {
      Project     = "spot-rl-seminar"
      ManagedBy   = "terraform"
      Environment = var.environment
    }
  }
}

# Lấy AZ list của region để map subnets.
data "aws_availability_zones" "available" {
  state = "available"
}
