# Controller EC2 instance — t3.small chạy Go controller container.
# User-data: install Docker, pull image, start container với env vars.

# Latest Amazon Linux 2023 AMI (kernel mới, support docker tốt)
data "aws_ami" "al2023" {
  most_recent = true
  owners      = ["amazon"]

  filter {
    name   = "name"
    values = ["al2023-ami-*-x86_64"]
  }
  filter {
    name   = "architecture"
    values = ["x86_64"]
  }
  filter {
    name   = "virtualization-type"
    values = ["hvm"]
  }
}

# User-data: bootstrap controller container.
# Render template với vars để inject SQS URL, subnet IDs, region, etc.
locals {
  controller_user_data = templatefile("${path.module}/user_data/controller.sh.tpl", {
    aws_region         = var.aws_region
    sqs_queue_url      = aws_sqs_queue.jobs.url
    subnet_az_a        = aws_subnet.public[0].id
    subnet_az_b        = aws_subnet.public[1].id
    subnet_az_c        = aws_subnet.public[2].id
    az_a               = data.aws_availability_zones.available.names[0]
    az_b               = data.aws_availability_zones.available.names[1]
    az_c               = data.aws_availability_zones.available.names[2]
    worker_sg_id       = aws_security_group.worker.id
    worker_iam_profile = aws_iam_instance_profile.worker.name
    shadow_mode        = var.enable_shadow_mode ? "true" : "false"
    image_tag          = var.controller_image_tag
    docker_image       = var.docker_image
    project_name       = var.project_name
  })
}

resource "aws_instance" "controller" {
  ami                    = data.aws_ami.al2023.id
  instance_type          = var.controller_instance_type
  subnet_id              = aws_subnet.public[0].id # AZ-a
  vpc_security_group_ids = [aws_security_group.controller.id]
  iam_instance_profile   = aws_iam_instance_profile.controller.name
  key_name               = var.key_pair_name

  user_data                   = local.controller_user_data
  user_data_replace_on_change = true # rebuild khi user-data thay đổi

  root_block_device {
    volume_size = 30 # GB — đủ cho Docker images
    volume_type = "gp3"
    encrypted   = true
  }

  metadata_options {
    http_tokens                 = "required" # IMDSv2 only — security best practice
    http_put_response_hop_limit = 2
  }

  tags = {
    Name      = "${var.project_name}-controller"
    Role      = "controller"
    ManagedBy = "spot-rl"
  }
}
