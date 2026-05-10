#!/bin/bash
# jenkins_master.sh.tpl — Bootstrap Jenkins Master trên Ubuntu 22.04
# Chạy 1 lần khi Droplet/EC2 boot. Logs: /var/log/cloud-init-output.log
#
# Template vars:
#   jenkins_agent_secret  — pre-shared secret cho agent connect (tùy chọn)
#   jenkins_admin_user    — tài khoản admin mặc định
#   jenkins_admin_pass    — password admin mặc định (đổi ngay sau setup)

set -euo pipefail
exec > >(tee /var/log/jenkins-bootstrap.log) 2>&1

echo "==================================================="
echo " Jenkins Master Bootstrap"
echo "==================================================="
date

# ── 1. System update ─────────────────────────────────
echo "[1/7] Updating system..."
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get upgrade -y -qq

# ── 2. OpenJDK 17 ────────────────────────────────────
echo "[2/7] Installing OpenJDK 17..."
apt-get install -y -qq openjdk-17-jdk curl wget gnupg2 git unzip

export JAVA_HOME=$(dirname $(dirname $(readlink -f $(which java))))
echo "JAVA_HOME=$JAVA_HOME"

# ── 3. Jenkins LTS ───────────────────────────────────
echo "[3/7] Installing Jenkins LTS..."
curl -fsSL https://pkg.jenkins.io/debian-stable/jenkins.io-2023.key \
    | gpg --dearmor -o /usr/share/keyrings/jenkins-keyring.gpg

echo "deb [signed-by=/usr/share/keyrings/jenkins-keyring.gpg] \
    https://pkg.jenkins.io/debian-stable binary/" \
    > /etc/apt/sources.list.d/jenkins.list

apt-get update -qq
apt-get install -y -qq jenkins

# ── 4. Tune JVM heap ─────────────────────────────────
echo "[4/7] Tuning JVM heap..."
mkdir -p /etc/systemd/system/jenkins.service.d
cat > /etc/systemd/system/jenkins.service.d/override.conf << 'EOF'
[Service]
Environment="JAVA_OPTS=-Xmx1g -Xms256m -XX:+UseG1GC -Djenkins.install.runSetupWizard=false"
EOF

systemctl daemon-reload

# ── 5. Pre-configure Jenkins (skip wizard) ───────────
echo "[5/7] Pre-configuring Jenkins..."

# Tạo thư mục jenkins home nếu chưa có
mkdir -p /var/lib/jenkins/init.groovy.d
chown -R jenkins:jenkins /var/lib/jenkins

# Groovy script: tạo admin user + disable setup wizard
cat > /var/lib/jenkins/init.groovy.d/01-create-admin.groovy << GROOVY
import jenkins.model.*
import hudson.security.*

def instance = Jenkins.getInstance()

// Tắt setup wizard
instance.setInstallState(InstallState.INITIAL_SETUP_COMPLETED)

// Tạo admin user
def hudsonRealm = new HudsonPrivateSecurityRealm(false)
hudsonRealm.createAccount("${jenkins_admin_user}", "${jenkins_admin_pass}")
instance.setSecurityRealm(hudsonRealm)

// Full control cho admin
def strategy = new FullControlOnceLoggedInAuthorizationStrategy()
strategy.setAllowAnonymousRead(false)
instance.setAuthorizationStrategy(strategy)

instance.save()
println "Admin user '${jenkins_admin_user}' created."
GROOVY

# Groovy script: mở JNLP port 50000 cho agent kết nối
cat > /var/lib/jenkins/init.groovy.d/02-agent-port.groovy << 'GROOVY'
import jenkins.model.*

def instance = Jenkins.getInstance()
instance.setSlaveAgentPort(50000)
instance.save()
println "Agent JNLP port set to 50000."
GROOVY

# Groovy script: tắt built-in node (chỉ chạy jobs trên agents)
cat > /var/lib/jenkins/init.groovy.d/03-disable-builtin-node.groovy << 'GROOVY'
import jenkins.model.*

def instance = Jenkins.getInstance()
instance.setNumExecutors(0)
instance.save()
println "Built-in node executors set to 0 (jobs run on agents only)."
GROOVY

chown -R jenkins:jenkins /var/lib/jenkins/init.groovy.d

# ── 6. Firewall ───────────────────────────────────────
echo "[6/7] Configuring firewall..."
ufw allow OpenSSH
ufw allow 8080/tcp    # Jenkins Web UI
ufw allow 50000/tcp   # Jenkins agent JNLP inbound
ufw --force enable

# ── 7. Start Jenkins ──────────────────────────────────
echo "[7/7] Starting Jenkins..."
systemctl enable jenkins
systemctl start jenkins

# Đợi Jenkins ready (tối đa 120s)
echo "  Waiting for Jenkins to be ready..."
timeout 120 bash -c '
    until curl -s -o /dev/null -w "%{http_code}" http://localhost:8080/login | grep -q "200\|403"; do
        echo "  ... still starting"
        sleep 5
    done
'

# ── Done ──────────────────────────────────────────────
PUBLIC_IP=$(curl -s http://169.254.169.254/metadata/v1/interfaces/public/0/ipv4/address 2>/dev/null \
    || curl -s ifconfig.me 2>/dev/null \
    || hostname -I | awk '{print $1}')

echo ""
echo "==================================================="
echo " Jenkins is ready!"
echo "==================================================="
echo ""
echo "  URL:       http://$${PUBLIC_IP}:8080"
echo "  User:      ${jenkins_admin_user}"
echo "  Pass:      ${jenkins_admin_pass}"
echo "  Agent port: 50000"
echo ""
echo "  Log: /var/log/jenkins-bootstrap.log"
echo "  Jenkins log: journalctl -u jenkins -f"
echo "==================================================="
