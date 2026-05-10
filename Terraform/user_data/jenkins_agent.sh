#!/bin/bash
# jenkins_agent.sh — UserData cho Spot instance tự join Jenkins làm agent
# Log: /var/log/jenkins-agent-bootstrap.log

set -euo pipefail
exec > >(tee /var/log/jenkins-agent-bootstrap.log) 2>&1

echo "=== Jenkins Agent Bootstrap ==="
date

# install awscli
apt-get update -qq
apt-get install -y -qq curl unzip

# ────── Cài đặt awscli ───────────────────────────────────
if ! command -v aws &> /dev/null; then
    echo "Installing AWS CLI v2..."
    curl "https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip" -o "awscliv2.zip"
    apt-get install -y -qq unzip
    unzip -q awscliv2.zip
    ./aws/install
else
    echo "AWS CLI already exists, updating..."
    curl "https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip" -o "awscliv2.zip"
    apt-get install -y -qq unzip
    unzip -q -o awscliv2.zip # Thêm -o để ghi đè (overwrite) khi giải nén
    ./aws/install --update    # Thêm --update để không bị hỏi y/n
fi

# ── 1. EC2 metadata (IMDSv2) ──────────────────────────
# IMDSv2 requires a session token — plain curl (IMDSv1) sẽ bị block
# nếu instance enforce http_tokens=required.
IMDS_TOKEN=$(curl -s -X PUT \
    -H "X-aws-ec2-metadata-token-ttl-seconds: 60" \
    http://169.254.169.254/latest/api/token)
imds() { curl -s -H "X-aws-ec2-metadata-token: $IMDS_TOKEN" \
    "http://169.254.169.254/latest/meta-data/$1"; }

INSTANCE_ID=$(imds instance-id)
AZ=$(imds placement/availability-zone)
REGION=$(imds placement/region)
AGENT_NAME="spot-agent-${INSTANCE_ID}"
echo "instance : $INSTANCE_ID  az : $AZ  region : $REGION  agent : $AGENT_NAME"

# ── 2. SSM Parameter Store ────────────────────────────
JENKINS_URL=$(aws ssm get-parameter --region "$REGION" \
    --name /spot-rl/jenkins-url --query Parameter.Value --output text)
JENKINS_USER=$(aws ssm get-parameter --region "$REGION" \
    --name /spot-rl/jenkins-user --query Parameter.Value --output text)
JENKINS_TOKEN=$(aws ssm get-parameter --region "$REGION" \
    --name /spot-rl/jenkins-token --with-decryption --query Parameter.Value --output text)
echo "jenkins : $JENKINS_URL"

# ── 3. Install Java 21 ───────────────────────────────
# Java version phải >= version Jenkins Master dùng.
# class file 65.0 = Java 21, 61.0 = Java 17 — dùng sai version → UnsupportedClassVersionError
echo "[3] Installing Java 21..."
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq openjdk-21-jre curl

# ── 4. Tạo Jenkins node qua Script Console ───────────
echo "[4] Waiting for Jenkins to be ready..."
# Jenkins mất 1-2 phút để boot hoàn toàn sau khi instance start.
# Retry tối đa 24 lần × 10s = 4 phút trước khi bail.
JENKINS_READY=false
for i in $(seq 1 24); do
    HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
        -u "$JENKINS_USER:$JENKINS_TOKEN" \
        "$JENKINS_URL/crumbIssuer/api/json" 2>/dev/null || echo "000")
    if [ "$HTTP_CODE" = "200" ]; then
        JENKINS_READY=true
        echo "  Jenkins ready after ${i}×10s"
        break
    fi
    echo "  Jenkins not ready yet (HTTP $HTTP_CODE, attempt $i/24)..." && sleep 10
done
[ "$JENKINS_READY" = "true" ] || { echo "ERROR: Jenkins not reachable after 4 min"; exit 1; }

echo "[4] Creating Jenkins node: $AGENT_NAME ..."

CRUMB=$(curl -sf -u "$JENKINS_USER:$JENKINS_TOKEN" \
    "$JENKINS_URL/crumbIssuer/api/json" \
    | python3 -c "
import sys,json; d=json.load(sys.stdin)
print(d['crumbRequestField']+':'+d['crumb'])")

# Tạo node DumbSlave với JNLP launcher
curl -sf -X POST -u "$JENKINS_USER:$JENKINS_TOKEN" -H "$CRUMB" \
    "$JENKINS_URL/scriptText" --data-urlencode "script=
import jenkins.model.*; import hudson.model.*; import hudson.slaves.*
def inst = Jenkins.getInstance()
def name = '$AGENT_NAME'
if (inst.getNode(name) != null) { println 'exists'; return }
def node = new DumbSlave(name, '/var/jenkins', new JNLPLauncher(true))
node.setNumExecutors(2)
node.setLabelString('spot-worker')
node.setMode(Node.Mode.NORMAL)
node.setRetentionStrategy(new RetentionStrategy.Always())
inst.addNode(node); inst.save()
println 'created'"

# Lấy secret qua REST API — chính xác hơn Script Console (không bị deprecated)
AGENT_SECRET=$(curl -sf -u "$JENKINS_USER:$JENKINS_TOKEN" \
    "$JENKINS_URL/computer/${AGENT_NAME}/slave-agent.jnlp" \
    | grep -oP '(?<=<argument>)[0-9a-f]{64}(?=</argument>)' \
    | head -1)

# Fallback: nếu JNLP endpoint không trả về (Jenkins 2.x dùng WebSocket)
if [ -z "$AGENT_SECRET" ]; then
    AGENT_SECRET=$(curl -sf -u "$JENKINS_USER:$JENKINS_TOKEN" -H "$CRUMB" \
        "$JENKINS_URL/scriptText" --data-urlencode "script=
import jenkins.model.*
def node = Jenkins.get().getNode('${AGENT_NAME}')
println node.getComputer().getJnlpMac()" \
        | tr -d '[:space:]')
fi

echo "[4] Node created, secret=${AGENT_SECRET:0:8}..."  # log 8 ký tự đầu để verify

# ── 5. Download agent.jar ────────────────────────────
echo "[5] Downloading agent.jar..."
mkdir -p /var/jenkins
curl -sL "$JENKINS_URL/jnlpJars/agent.jar" -o /var/jenkins/agent.jar

# ── 6. Systemd service ───────────────────────────────
echo "[6] Setting up systemd service..."
cat > /etc/systemd/system/jenkins-agent.service << EOF
[Unit]
Description=Jenkins Spot Agent ${AGENT_NAME}
After=network-online.target

[Service]
Type=simple
WorkingDirectory=/var/jenkins
ExecStart=/usr/bin/java -jar /var/jenkins/agent.jar \
    -url ${JENKINS_URL} \
    -secret ${AGENT_SECRET} \
    -name ${AGENT_NAME} \
    -workDir /var/jenkins
Restart=on-failure
RestartSec=15s
StandardOutput=append:/var/log/jenkins-agent.log
StandardError=append:/var/log/jenkins-agent.log

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable jenkins-agent
systemctl start jenkins-agent

# ── 7. Đợi agent online ──────────────────────────────
echo "[7] Waiting for agent to come online..."
for i in $(seq 1 12); do
    OFFLINE=$(curl -sf -u "$JENKINS_USER:$JENKINS_TOKEN" \
        "$JENKINS_URL/computer/${AGENT_NAME}/api/json?tree=offline" \
        | python3 -c "import sys,json; print(json.load(sys.stdin).get('offline','true'))" \
        2>/dev/null || echo "true")
    [ "$OFFLINE" = "False" ] && echo "  Agent ONLINE" && break
    echo "  waiting ($i/12)..." && sleep 5
done

# ── 8. Tag instance để controller track ──────────────
aws ec2 create-tags --region "$REGION" --resources "$INSTANCE_ID" \
    --tags \
        Key=JenkinsAgentName,Value="$AGENT_NAME" \
        Key=JenkinsRole,Value=agent \
        Key=ManagedBy,Value=spot-rl-controller

echo ""
echo "=== Done: $AGENT_NAME online ==="
echo "Log: journalctl -u jenkins-agent -f"