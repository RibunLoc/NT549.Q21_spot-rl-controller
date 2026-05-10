# Variables — override qua terraform.tfvars hoặc -var flags.

variable "aws_region" {
  description = "AWS region — phải có 3 AZs cho multi-AZ demo"
  type        = string
  default     = "ap-southeast-1" # Singapore: 3 AZs (a, b, c)
}

variable "environment" {
  description = "Environment tag (dev/staging/prod)"
  type        = string
  default     = "demo"
}

variable "project_name" {
  description = "Prefix cho mọi resource"
  type        = string
  default     = "spot-rl"
}

variable "vpc_cidr" {
  description = "VPC CIDR block"
  type        = string
  default     = "10.0.0.0/16"
}

variable "controller_instance_type" {
  description = "Instance type cho controller (cần đủ RAM cho ONNX runtime)"
  type        = string
  default     = "t3.small" # 2 vCPU, 2GB RAM — đủ cho controller
}

variable "ssh_allowed_cidr" {
  description = "CIDR được phép SSH vào controller (set IP thật của bạn)"
  type        = string
  default     = "0.0.0.0/0" # ⚠️ Override trong tfvars để hạn chế
}

variable "key_pair_name" {
  description = "EC2 key pair có sẵn (tạo trước qua AWS console)"
  type        = string
}

variable "monthly_budget_usd" {
  description = "Hard budget cap — alert khi vượt"
  type        = number
  default     = 100
}

variable "alert_email" {
  description = "Email nhận alert budget + safety violations"
  type        = string
}

variable "controller_image_tag" {
  description = "Docker image tag cho controller (push trước khi apply)"
  type        = string
  default     = "latest"
}

variable "docker_image" {
  description = "Docker image name, e.g. yourdockerhub/spot-rl-controller"
  type        = string
  default     = "spot-rl-controller"
}

variable "enable_shadow_mode" {
  description = "true = controller log decisions, KHÔNG execute action thật"
  type        = bool
  default     = true # an toàn mặc định
}

variable "jenkins_admin_user" {
  description = "Jenkins admin username"
  type        = string
  default     = "admin"
}

variable "jenkins_admin_pass" {
  description = "Jenkins admin password — override trong tfvars, không để default yếu"
  type        = string
  sensitive   = true
}

variable "jenkins_master_url" {
  description = "Jenkins Master URL — dùng public IP khi Master chạy ngoài AWS (DO droplet, v.v.)"
  type        = string
  default     = "http://REPLACE_WITH_DROPLET_IP:8080"
}
