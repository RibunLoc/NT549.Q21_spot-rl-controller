# Outputs — sau khi `terraform apply`, in ra để copy vào .env / scripts.

output "controller_public_ip" {
  description = "Public IP của controller — SSH vào đây"
  value       = aws_instance.controller.public_ip
}

output "controller_instance_id" {
  description = "EC2 instance ID"
  value       = aws_instance.controller.id
}

output "ssh_command" {
  description = "Lệnh SSH vào controller"
  value       = "ssh -i ~/.ssh/${var.key_pair_name}.pem ec2-user@${aws_instance.controller.public_ip}"
}

output "vpc_id" {
  description = "VPC ID"
  value       = aws_vpc.main.id
}

output "subnet_ids_by_az" {
  description = "Subnet IDs per AZ — copy vào controller .env"
  value = {
    for i, az in data.aws_availability_zones.available.names : az => aws_subnet.public[i].id
    if i < 3
  }
}

output "sqs_queue_url" {
  description = "SQS queue URL cho workload jobs"
  value       = aws_sqs_queue.jobs.url
}

output "sqs_queue_arn" {
  description = "SQS queue ARN"
  value       = aws_sqs_queue.jobs.arn
}

output "controller_security_group_id" {
  description = "SG ID của controller"
  value       = aws_security_group.controller.id
}

output "worker_security_group_id" {
  description = "SG ID cho worker nodes — controller dùng khi launch instance"
  value       = aws_security_group.worker.id
}

output "worker_iam_profile_name" {
  description = "IAM instance profile name cho worker nodes"
  value       = aws_iam_instance_profile.worker.name
}

output "alert_topic_arn" {
  description = "SNS topic cho alerts — confirm subscription qua email"
  value       = aws_sns_topic.alerts.arn
}

output "monthly_budget" {
  description = "Monthly budget cap"
  value       = "$${var.monthly_budget_usd} USD"
}

output "jenkins_url" {
  description = "Jenkins Master Web UI (public — dùng trên browser)"
  value       = "http://${aws_eip.jenkins_master.public_ip}:8080"
}

output "jenkins_url_internal" {
  description = "Jenkins Master URL nội bộ VPC — agents dùng cái này (lưu trong SSM)"
  value       = "http://${aws_instance.jenkins_master.private_ip}:8080"
}

output "jenkins_ssh" {
  description = "SSH vào Jenkins Master"
  value       = "ssh -i ~/.ssh/${var.key_pair_name}.pem ubuntu@${aws_eip.jenkins_master.public_ip}"
}

output "jenkins_admin_user" {
  description = "Jenkins admin username"
  value       = var.jenkins_admin_user
}

output "jenkins_agent_port" {
  description = "JNLP port cho Spot agent kết nối về Master"
  value       = "50000"
}

output "jenkins_agent_userdata_b64" {
  description = "Base64 UserData cho Spot agent — dùng khi controller launch instance thủ công"
  value       = base64encode(data.template_file.jenkins_agent_userdata.rendered)
  sensitive   = false
}

output "ssm_param_jenkins_url" {
  description = "SSM param path chứa Jenkins URL"
  value       = aws_ssm_parameter.jenkins_url.name
}

output "ssm_param_jenkins_token" {
  description = "SSM param path chứa Jenkins API token (SecureString)"
  value       = aws_ssm_parameter.jenkins_token.name
}

output "next_steps" {
  description = "Hướng dẫn sau apply"
  value       = <<-EOT

  ╔══════════════════════════════════════════════════════════════════╗
  ║  Setup hoàn tất! Các bước tiếp theo:                              ║
  ╠══════════════════════════════════════════════════════════════════╣
  ║                                                                    ║
  ║  1. Confirm SNS email:                                             ║
  ║     Mở email ${var.alert_email} → click confirm subscription      ║
  ║                                                                    ║
  ║  2. SSH vào controller:                                            ║
  ║     ssh -i ~/.ssh/${var.key_pair_name}.pem \\                     ║
  ║         ec2-user@${aws_instance.controller.public_ip}             ║
  ║                                                                    ║
  ║  3. Upload model ONNX:                                             ║
  ║     scp results/best_model.onnx \\                                 ║
  ║         ec2-user@${aws_instance.controller.public_ip}:/opt/spot-rl/model.onnx
  ║                                                                    ║
  ║  4. Build + push Docker image controller (từ máy local):          ║
  ║     cd spot-rl-controller                                          ║
  ║     docker build -t spot-rl-controller:latest .                    ║
  ║     # Push lên DockerHub hoặc save tarball:                        ║
  ║     docker save spot-rl-controller:latest | gzip > controller.tgz  ║
  ║     scp controller.tgz ec2-user@<ip>:~/                            ║
  ║                                                                    ║
  ║  5. Trên controller, load image + start service:                   ║
  ║     gunzip -c controller.tgz | docker load                         ║
  ║     sudo systemctl enable --now spot-rl-controller                 ║
  ║     journalctl -u spot-rl-controller -f                            ║
  ║                                                                    ║
  ║  6. SHADOW MODE đang ${var.enable_shadow_mode ? "ON" : "OFF"} — controller chỉ log decisions      ║
  ║     Sau khi validate, tắt shadow mode trong /etc/spot-rl/controller.env
  ║                                                                    ║
  ║  Kill switch (emergency stop):                                     ║
  ║     ssh ... 'sudo touch /etc/spot-rl/killswitch'                  ║
  ║                                                                    ║
  ╚══════════════════════════════════════════════════════════════════╝
  EOT
}
