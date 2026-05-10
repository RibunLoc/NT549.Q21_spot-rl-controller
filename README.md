# spot-rl-controller

Production controller cho hệ thống **Spot Instance RL Optimization**.  
Nhận ONNX model được train từ [spot-rl-optimiztion](https://github.com/RibunLoc/spot-rl-optimiztion), thực thi các action quản lý EC2 Spot/On-Demand theo thời gian thực.

---

## Kiến trúc tổng thể

```
┌─────────────────────────────────────────────────────────────────┐
│                    spot-rl-optimiztion (repo kia)               │
│                                                                  │
│  SpotOrchestratorEnv  →  Factored Dueling DQN  →  best_model.onnx │
└──────────────────────────────┬──────────────────────────────────┘
                               │  export ONNX
                               ▼
┌─────────────────────────────────────────────────────────────────┐
│                    spot-rl-controller (repo này)                 │
│                                                                  │
│  AWS APIs ──► state/collector.go ──► [45-dim state vector]      │
│                                           │                      │
│                                           ▼                      │
│                              inference/model.go (ONNX Runtime)  │
│                                           │                      │
│                                    [121 Q-values]                │
│                                           │ argmax + mask        │
│                                           ▼                      │
│                              safety/limits.go (guardrails)      │
│                                           │                      │
│                                           ▼                      │
│                              action/executor.go                  │
│                              ├── EC2: spot/on-demand ops        │
│                              ├── Jenkins: drain agent           │
│                              └── registry: track instances      │
│                                           │                      │
│                              metrics/reporter.go                 │
│                              └── CloudWatch / Prometheus        │
└─────────────────────────────────────────────────────────────────┘
                               │
                               ▼
┌─────────────────────────────────────────────────────────────────┐
│                    Jenkins + Spot Agents                         │
│                                                                  │
│  Jenkins Master (DO Droplet / EC2 On-Demand)                    │
│       │  JNLP :50000                                            │
│       └── spot-agent-<instance-id>  (EC2 Spot, UserData auto-join) │
└─────────────────────────────────────────────────────────────────┘
```

---

## Quan hệ với repo spot-rl-optimiztion

| Repo | Vai trò |
|------|---------|
| [spot-rl-optimiztion](https://github.com/RibunLoc/spot-rl-optimiztion) | Train RL agent (Python, Gymnasium, PyTorch) — output `best_model.onnx` |
| **spot-rl-controller** (repo này) | Runtime controller (Go) — load ONNX, thực thi action trên AWS thật |

**Workflow:**
```
1. Train xong trên Kaggle → export ONNX
   scp results/best_model.onnx ec2-user@<controller-ip>:/opt/spot-rl/model.onnx

2. Controller load model → chạy control loop mỗi 5 phút
   MODEL_PATH=/opt/spot-rl/model.onnx ./spot-rl-controller

3. Mỗi loop: collect state → predict action → safety check → execute EC2 API
```

**Action space (121 actions = 8 ops × 15 pools + 1 HOLD):**

| Op | Mô tả |
|----|-------|
| `PROVISION_SPOT` | Launch Spot instance mới |
| `PROVISION_ONDEMAND` | Launch On-Demand instance mới |
| `RELEASE_SPOT` | Terminate 1 Spot instance |
| `RELEASE_ONDEMAND` | Terminate 1 On-Demand instance |
| `CONVERT_TO_ONDEMAND` | Terminate Spot → Launch OD (cùng AZ) |
| `CONVERT_TO_SPOT` | Terminate OD → Launch Spot (cùng AZ) |
| `REBALANCE_SPOT` | Migrate Spot sang pool rẻ hơn (drain Jenkins agent trước) |
| `RESERVE_CAPACITY` | Pre-allocate OD khi interrupt risk cao |
| `HOLD` | Không làm gì |

---

## Jenkins + Spot Agent Integration

Controller tích hợp Jenkins để **drain gracefully** trước khi terminate Spot instance:

```
REBALANCE_SPOT action:
  1. Provision Spot mới ở pool đích
  2. DrainAgent() — mark offline, đợi jobs finish (timeout 90s)
  3. TerminateInstance — terminate Spot cũ
  4. DeleteAgent — xóa node khỏi Jenkins
```

Spot instance tự động join Jenkins khi boot qua `Terraform/user_data/jenkins_agent.sh`:
- Đọc credentials từ **SSM Parameter Store** (không hardcode)
- Tạo JNLP node trên Jenkins Master qua Script Console API
- Systemd service `jenkins-agent` tự restart nếu mất kết nối
- Tag EC2 `JenkinsAgentName=spot-agent-<instance-id>` để controller track

---

## Cấu trúc repo

```
spot-rl-controller/
├── cmd/
│   ├── controller/     # Main control loop
│   ├── worker/         # SQS job worker (chạy trên Spot agent)
│   ├── producer/       # Test: sinh jobs vào SQS
│   └── sender/         # Test: gửi metrics
├── internal/
│   ├── action/         # Executor: decode action → AWS API
│   ├── aws/            # EC2, SQS, Pricing clients
│   ├── inference/      # ONNX Runtime wrapper
│   ├── jenkins/        # Jenkins API client (drain, delete agent)
│   ├── registry/       # Instance registry (EC2 tag → agent name)
│   ├── safety/         # Guardrails: max cost, max instances
│   └── state/          # State collector + action mask
├── pkg/types/          # Shared types (action space, pool info)
├── Terraform/          # AWS infrastructure
│   ├── jenkins.tf      # Jenkins Master EC2 + SSM params
│   ├── iam.tf          # IAM roles (controller + worker)
│   ├── network.tf      # VPC, subnets, AZs
│   └── user_data/
│       ├── jenkins_master.sh.tpl   # Bootstrap Jenkins Master
│       └── jenkins_agent.sh(.tpl) # Bootstrap Spot → Jenkins agent
└── configs/
    └── .env.example    # Template env vars (không commit .env thật)
```

---

## Quickstart

### 1. Infrastructure
```bash
cd Terraform
cp terraform.tfvars.example terraform.tfvars
# Edit: aws_region, key_pair_name, jenkins_master_url, jenkins_admin_pass
terraform init && terraform apply
```

### 2. SSM Parameters (Jenkins credentials)
```bash
aws ssm put-parameter --name /spot-rl/jenkins-url \
  --value "https://your-jenkins-domain" --type String
aws ssm put-parameter --name /spot-rl/jenkins-user \
  --value "admin" --type String
aws ssm put-parameter --name /spot-rl/jenkins-token \
  --value "<api-token>" --type SecureString
```

### 3. Deploy controller
```bash
# Copy ONNX model từ repo training
scp ../spot-rl-optimiztion/results/best_model.onnx \
    ec2-user@<controller-ip>:/opt/spot-rl/model.onnx

# Build + run
go build -o bin/controller ./cmd/controller
SHADOW_MODE=true ./bin/controller   # dry-run trước
```

### 4. Bật live mode
```bash
# Sau khi validate shadow mode log ổn
SHADOW_MODE=false ./bin/controller
```

### Kill switch (emergency)
```bash
ssh ec2-user@<controller-ip> 'sudo touch /etc/spot-rl/killswitch'
```

---

## Environment Variables

| Var | Default | Mô tả |
|-----|---------|-------|
| `MODEL_PATH` | `models/dqn_stable.onnx` | Path tới ONNX model |
| `SHADOW_MODE` | `true` | `true` = chỉ log, không execute |
| `LOOP_INTERVAL_SEC` | `300` | Chu kỳ control loop (giây) |
| `JENKINS_URL` | `` | Jenkins Master URL |
| `JENKINS_USER` | `` | Jenkins admin username |
| `JENKINS_TOKEN` | `` | Jenkins API token |
| `AMI_ID` | `` | AMI cho Spot/OD instances |
| `SQS_QUEUE_URL` | `` | SQS queue URL (job workload) |
| `SUBNET_AZ_A/B/C` | `` | Subnet IDs cho 3 AZs |
