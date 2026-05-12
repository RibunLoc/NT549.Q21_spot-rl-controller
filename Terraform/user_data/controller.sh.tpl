#!/bin/bash
# Bootstrap script cho controller node.
# Chạy 1 lần khi instance launch. Logs: /var/log/cloud-init-output.log

set -euo pipefail
exec > >(tee /var/log/spot-rl-bootstrap.log) 2>&1

echo "=== Spot RL Controller Bootstrap ==="
date

# 1. Update + install Docker
dnf update -y
dnf install -y docker awscli amazon-cloudwatch-agent
systemctl enable --now docker
usermod -aG docker ec2-user

# 2. Tạo dirs
mkdir -p /etc/spot-rl /var/log/spot-rl /opt/spot-rl

# 3. Config file cho controller (env vars)
cat > /etc/spot-rl/controller.env <<EOF
# AWS region + SQS
AWS_REGION=${aws_region}
SQS_QUEUE_URL=${sqs_queue_url}

# Subnets per AZ — controller cần để launch worker nodes
SUBNET_AZ_A=${subnet_az_a}
SUBNET_AZ_B=${subnet_az_b}
SUBNET_AZ_C=${subnet_az_c}

# Worker config — controller dùng launch template để launch spot workers
WORKER_SECURITY_GROUP=${worker_sg_id}
WORKER_IAM_PROFILE=${worker_iam_profile}
WORKER_LAUNCH_TEMPLATE=${worker_launch_template}

# Controller behavior
SHADOW_MODE=${shadow_mode}
LOOP_INTERVAL_SEC=900
EPISODE_STEPS=672
MODEL_PATH=/opt/spot-rl/model.onnx

# Safety
KILL_SWITCH_PATH=/etc/spot-rl/killswitch
EOF

chmod 644 /etc/spot-rl/controller.env

# 4. Download model từ S3 (cần upload trước)
# TODO: setup S3 bucket cho models, hoặc dùng systemd-tmpfiles
# Tạm thời để placeholder — user copy model lên thủ công
touch /opt/spot-rl/model.onnx.placeholder
echo "TODO: scp model.onnx vào /opt/spot-rl/model.onnx trước khi start container"

# 5. Pull controller image từ Docker Hub
docker pull ${docker_image}:${image_tag} || true
# Nếu không có internet, dùng: docker load -i /opt/spot-rl/controller.tar

# 6. Tạo systemd service
cat > /etc/systemd/system/spot-rl-controller.service <<'EOF'
[Unit]
Description=Spot RL Controller
After=docker.service network-online.target
Requires=docker.service

[Service]
Type=simple
EnvironmentFile=/etc/spot-rl/controller.env
ExecStartPre=-/usr/bin/docker rm -f spot-rl-controller
ExecStart=/usr/bin/docker run --rm --name spot-rl-controller \
  --env-file /etc/spot-rl/controller.env \
  -v /opt/spot-rl:/opt/spot-rl:ro \
  -v /etc/spot-rl:/etc/spot-rl:ro \
  -v /var/log/spot-rl:/var/log/spot-rl \
  ${docker_image}:${image_tag}
Restart=on-failure
RestartSec=30s
StandardOutput=append:/var/log/spot-rl/controller.log
StandardError=append:/var/log/spot-rl/controller.log

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload

# Chú ý: KHÔNG enable service ngay — đợi user upload model + image trước.
# User sẽ chạy: sudo systemctl enable --now spot-rl-controller
echo ""
echo "=== Bootstrap done ==="
echo "Next steps (SSH vào instance):"
echo "  1. scp model.onnx ec2-user@<this-instance>:/opt/spot-rl/model.onnx"
echo "  2. docker load -i spot-rl-controller.tar  (hoặc docker pull)"
echo "  3. sudo systemctl enable --now spot-rl-controller"
echo "  4. journalctl -u spot-rl-controller -f"
