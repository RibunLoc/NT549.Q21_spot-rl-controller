# Security groups: controller + worker nodes.
# Controller cần: SSH inbound, all egress.
# Worker cần: SSH từ controller, all egress (để job process gọi API ngoài).

resource "aws_security_group" "controller" {
  name        = "${var.project_name}-controller-sg"
  description = "Spot RL controller node"
  vpc_id      = aws_vpc.main.id

  # SSH từ IP của bạn
  ingress {
    description = "SSH"
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = [var.ssh_allowed_cidr]
  }

  # Prometheus metrics endpoint (optional, từ public IP)
  ingress {
    description = "Prometheus metrics"
    from_port   = 9090
    to_port     = 9090
    protocol    = "tcp"
    cidr_blocks = [var.ssh_allowed_cidr]
  }

  egress {
    description = "All outbound"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = {
    Name = "${var.project_name}-controller-sg"
  }
}

resource "aws_security_group" "worker" {
  name        = "${var.project_name}-worker-sg"
  description = "Spot RL worker nodes (managed by controller)"
  vpc_id      = aws_vpc.main.id

  # SSH chỉ từ controller (debug)
  ingress {
    description     = "SSH from controller"
    from_port       = 22
    to_port         = 22
    protocol        = "tcp"
    security_groups = [aws_security_group.controller.id]
  }

  egress {
    description = "All outbound"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = {
    Name = "${var.project_name}-worker-sg"
  }
}
