# jenkins.tf — Jenkins Master trên EC2 On-Demand
# Dùng OD (không phải Spot) vì Master phải luôn available
# Instance: t3.small (2 vCPU / 2GB) — đủ cho demo

# ── User Data ─────────────────────────────────────────
locals {
  jenkins_user_data = templatefile("${path.module}/user_data/jenkins_master.sh.tpl", {
    jenkins_admin_user = var.jenkins_admin_user
    jenkins_admin_pass = var.jenkins_admin_pass
  })
}

# ── Security Group ────────────────────────────────────
resource "aws_security_group" "jenkins_master" {
  name        = "${var.project_name}-jenkins-master"
  description = "Jenkins Master: UI port 8080, agent JNLP port 50000"
  vpc_id      = aws_vpc.main.id

  # Web UI từ IP của bạn
  ingress {
    description = "Jenkins UI from admin"
    from_port   = 8080
    to_port     = 8080
    protocol    = "tcp"
    cidr_blocks = [var.ssh_allowed_cidr]
  }

  # Web UI từ Spot agents trong VPC — cần để bootstrap (crumb + scriptText + agent.jar)
  ingress {
    description = "Jenkins UI from VPC (agents)"
    from_port   = 8080
    to_port     = 8080
    protocol    = "tcp"
    cidr_blocks = [var.vpc_cidr]
  }

  # Agent JNLP port — từ trong VPC (agents kết nối về Master)
  ingress {
    description = "Jenkins agent JNLP"
    from_port   = 50000
    to_port     = 50000
    protocol    = "tcp"
    cidr_blocks = [var.vpc_cidr]
  }

  # SSH
  ingress {
    description = "SSH"
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = [var.ssh_allowed_cidr]
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = {
    Name = "${var.project_name}-jenkins-master-sg"
  }
}

# ── Ubuntu 22.04 AMI ──────────────────────────────────
data "aws_ami" "ubuntu_22" {
  most_recent = true
  owners      = ["099720109477"] # Canonical

  filter {
    name   = "name"
    values = ["ubuntu/images/hvm-ssd/ubuntu-jammy-22.04-amd64-server-*"]
  }
  filter {
    name   = "virtualization-type"
    values = ["hvm"]
  }
}

# ── Jenkins Master EC2 ────────────────────────────────
resource "aws_instance" "jenkins_master" {
  ami                    = data.aws_ami.ubuntu_22.id
  instance_type          = "t3.small"
  subnet_id              = aws_subnet.public[0].id
  vpc_security_group_ids = [aws_security_group.jenkins_master.id]
  key_name               = var.key_pair_name

  user_data                   = local.jenkins_user_data
  user_data_replace_on_change = true

  root_block_device {
    volume_size = 20    # GB — Jenkins home + build artifacts
    volume_type = "gp3"
    encrypted   = true
  }

  metadata_options {
    http_tokens                 = "required" # IMDSv2
    http_put_response_hop_limit = 2
  }

  tags = {
    Name      = "${var.project_name}-jenkins-master"
    Role      = "jenkins-master"
    ManagedBy = "terraform"
  }
}

# ── Elastic IP ────────────────────────────────────────
# Giữ IP cố định khi stop/start instance
resource "aws_eip" "jenkins_master" {
  instance = aws_instance.jenkins_master.id
  domain   = "vpc"

  tags = {
    Name = "${var.project_name}-jenkins-master-eip"
  }
}

# ── SSM Parameters — Jenkins credentials cho Spot agents ──
# Agent UserData đọc 3 params này lúc boot, không hardcode trong script.
# /jenkins-url: public IP gắn EIP (stable)
# /jenkins-user + /jenkins-token: SecureString (encrypted)

resource "aws_ssm_parameter" "jenkins_url" {
  name = "/spot-rl/jenkins-url"
  type = "String"
  # Jenkins Master chạy ngoài AWS (DO droplet) — dùng public IP.
  # Update giá trị này thủ công sau khi có IP droplet:
  #   aws ssm put-parameter --name /spot-rl/jenkins-url \
  #     --value "http://<DO-droplet-ip>:8080" --overwrite
  value = var.jenkins_master_url

  tags = { Name = "${var.project_name}-jenkins-url" }
}

resource "aws_ssm_parameter" "jenkins_user" {
  name  = "/spot-rl/jenkins-user"
  type  = "String"
  value = var.jenkins_admin_user

  tags = { Name = "${var.project_name}-jenkins-user" }
}

resource "aws_ssm_parameter" "jenkins_token" {
  name  = "/spot-rl/jenkins-token"
  type  = "SecureString" # encrypted at rest
  value = var.jenkins_admin_pass

  lifecycle {
    ignore_changes = [value] # sau khi tạo API token thật, update thủ công qua AWS Console
  }

  tags = { Name = "${var.project_name}-jenkins-token" }
}

# ── Spot Agent User Data (template) ───────────────────
# Controller dùng data source này để launch Spot instances làm Jenkins agents.
# Credentials KHÔNG inject vào template — agent tự đọc từ SSM lúc runtime.
data "template_file" "jenkins_agent_userdata" {
  template = file("${path.module}/user_data/jenkins_agent.sh.tpl")
}
