# Terraform — Spot RL Demo Infrastructure

Hạ tầng AWS tối thiểu để chạy controller thật.

## Resources

| Resource | Cost ước tính | Mục đích |
|----------|---------------|----------|
| VPC + 3 subnets + IGW | $0 | Network |
| EC2 t3.small (controller) | ~$15/tháng | Chạy Go controller container |
| SQS queue | <$1/tháng | Workload jobs |
| CloudWatch logs | <$1/tháng | Controller logs |
| SNS alerts | $0 | Budget + safety alerts |
| **Worker nodes (managed by controller)** | $5-30/tháng | Spot/OD instances do agent quyết |
| **TỔNG** | **~$25-50/tháng** | (tùy lượng worker spawn) |

> ⚠️ Budget alert ở mức `monthly_budget_usd` (default $50). Vượt 100% sẽ có email cảnh báo.

## Setup

### Prerequisites

```bash
# 1. AWS CLI configured
aws sts get-caller-identity

# 2. Terraform >= 1.5
terraform version

# 3. Tạo EC2 key pair trong AWS Console
#    EC2 → Key Pairs → Create → tải file .pem về ~/.ssh/
chmod 400 ~/.ssh/your-key.pem
```

### Configure

```bash
cd terraform/
cp terraform.tfvars.example terraform.tfvars
# Edit terraform.tfvars với:
#   - ssh_allowed_cidr = "YOUR.PUBLIC.IP/32"
#   - key_pair_name    = "your-key-pair"
#   - alert_email      = "you@example.com"
```

### Apply

```bash
terraform init
terraform plan   # review changes
terraform apply  # gõ "yes" để confirm
```

Sau khi xong, output sẽ in hướng dẫn next steps.

## Architecture

```
┌─────────────────────────────────────────────┐
│ VPC 10.0.0.0/16                             │
│                                             │
│  ┌─────────────┐  ┌─────────────┐  ┌──────┐│
│  │ Subnet AZ-a │  │ Subnet AZ-b │  │AZ-c  ││
│  │ 10.0.1.0/24 │  │ 10.0.2.0/24 │  │.3.0  ││
│  └──────┬──────┘  └──────┬──────┘  └──┬───┘│
│         │                │             │    │
│         ▼                ▼             ▼    │
│   ┌──────────┐                              │
│   │Controller│  ◀── SSH (port 22)           │
│   │t3.small  │  ◀── Metrics (port 9090)     │
│   └────┬─────┘                              │
│        │ EC2 API                            │
│        ▼                                    │
│   ┌──────────┐  ┌──────────┐  ┌──────────┐ │
│   │ Worker 1 │  │ Worker 2 │  │ Worker N │ │
│   │ (spot)   │  │ (od)     │  │ (spot)   │ │
│   └──────────┘  └──────────┘  └──────────┘ │
│                                             │
└─────────────────────────────────────────────┘
       ↕                ↕                ↕
   ┌──────┐         ┌──────┐         ┌──────┐
   │ SQS  │         │ IAM  │         │ SNS  │
   │ jobs │         │roles │         │alerts│
   └──────┘         └──────┘         └──────┘
```

## Bộ controller lên EC2

Sau khi `terraform apply`:

```bash
# Lấy public IP từ output
CONTROLLER_IP=$(terraform output -raw controller_public_ip)

# 1. SSH vào
ssh -i ~/.ssh/your-key.pem ec2-user@$CONTROLLER_IP

# 2. Upload model (từ máy local)
scp -i ~/.ssh/your-key.pem ../results/best_model.onnx \
    ec2-user@$CONTROLLER_IP:/opt/spot-rl/model.onnx

# 3. Build + push Docker image (từ máy local)
cd ../spot-rl-controller
docker build -t spot-rl-controller:latest .
docker save spot-rl-controller:latest | gzip > /tmp/controller.tgz
scp -i ~/.ssh/your-key.pem /tmp/controller.tgz ec2-user@$CONTROLLER_IP:~/

# 4. Trên controller: load image + start
ssh -i ~/.ssh/your-key.pem ec2-user@$CONTROLLER_IP <<'EOF'
gunzip -c controller.tgz | docker load
sudo systemctl enable --now spot-rl-controller
journalctl -u spot-rl-controller -f
EOF
```

## Operations

### Xem logs
```bash
ssh ... 'journalctl -u spot-rl-controller -f'
ssh ... 'tail -f /var/log/spot-rl/controller.log'
```

### Tắt agent ngay (kill switch)
```bash
ssh ... 'sudo touch /etc/spot-rl/killswitch'
# Controller vẫn chạy nhưng safety guard chặn mọi action.
```

### Bật lại
```bash
ssh ... 'sudo rm /etc/spot-rl/killswitch'
```

### Tắt shadow mode (cho phép execute thật)
```bash
ssh ... 'sudo sed -i "s/SHADOW_MODE=true/SHADOW_MODE=false/" /etc/spot-rl/controller.env'
ssh ... 'sudo systemctl restart spot-rl-controller'
```

### Stop everything
```bash
# Terminate worker nodes do agent đã spawn (qua tag)
aws ec2 describe-instances \
  --filters "Name=tag:ManagedBy,Values=spot-rl" "Name=instance-state-name,Values=running" \
  --query "Reservations[].Instances[].InstanceId" --output text \
  | xargs -r aws ec2 terminate-instances --instance-ids

# Destroy infra
terraform destroy
```

## Safety Layers

| Layer | Cấp độ | Chặn gì |
|-------|--------|---------|
| 1. Shadow mode | Soft | Tất cả action thật |
| 2. Kill switch | Hard | Tất cả action (file flag) |
| 3. Safety guard (in-process) | Hard | Fleet >10, rate >30/h, cost >$5/h |
| 4. IAM policy | Hard | Chỉ terminate instance có tag `ManagedBy=spot-rl` |
| 5. AWS Budget alert | Soft | Email khi vượt 50/80/100% |
| 6. CloudWatch alarm | Soft | Email khi fleet >15 instances |

## Cost Optimization

Khi không demo:
```bash
# Stop controller (không tốn EC2, vẫn giữ state)
aws ec2 stop-instances --instance-ids $(terraform output -raw controller_instance_id)

# Restart khi cần
aws ec2 start-instances --instance-ids $(terraform output -raw controller_instance_id)
```

Hoặc destroy hoàn toàn:
```bash
terraform destroy
# Apply lại khi cần demo: terraform apply
```

## Troubleshooting

**Controller không start**: check `/var/log/spot-rl-bootstrap.log` qua SSH.

**Model không load**: kiểm tra `/opt/spot-rl/model.onnx` tồn tại và đúng schema (90/121).

**SQS empty nhưng controller báo busy**: dùng `aws sqs get-queue-attributes` xem ApproximateNumberOfMessages.

**Email alert không tới**: vào SNS topic → confirm subscription qua link trong email AWS gửi.
