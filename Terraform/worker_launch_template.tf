# Launch Template cho worker Spot instances.
# Controller dùng launch_template_id này khi gọi EC2 RunInstances —
# tách biệt hoàn toàn với controller instance (AL2023).
#
# AMI: Ubuntu 24 đã bake CloudWatch agent + Java 21 (từ jenkins_agent.sh)
# User-data: chỉ cần fetch CloudWatch config + join Jenkins — không cài lại agent

locals {
  worker_user_data = base64encode(templatefile(
    "${path.module}/user_data/jenkins_agent.sh",
    {}
  ))
}

resource "aws_launch_template" "worker" {
  name_prefix   = "${var.project_name}-worker-"
  image_id      = var.worker_ami_id
  # instance_type không set ở đây — controller truyền type cụ thể lúc RunInstances
  # để hỗ trợ multi-type (m5.large / c5.xlarge / ...)

  iam_instance_profile {
    name = aws_iam_instance_profile.worker.name
  }

  vpc_security_group_ids = [aws_security_group.worker.id]

  user_data = local.worker_user_data

  metadata_options {
    http_tokens                 = "required" # IMDSv2
    http_put_response_hop_limit = 2
  }

  block_device_mappings {
    device_name = "/dev/sda1"
    ebs {
      volume_size           = 20
      volume_type           = "gp3"
      encrypted             = true
      delete_on_termination = true
    }
  }

  tag_specifications {
    resource_type = "instance"
    tags = {
      Name      = "${var.project_name}-worker"
      Role      = "worker"
      ManagedBy = "spot-rl-controller"
    }
  }

  tag_specifications {
    resource_type = "volume"
    tags = {
      Name      = "${var.project_name}-worker-vol"
      ManagedBy = "spot-rl-controller"
    }
  }

  tags = {
    Name      = "${var.project_name}-worker-lt"
    ManagedBy = "spot-rl"
  }

  lifecycle {
    create_before_destroy = true
  }
}
