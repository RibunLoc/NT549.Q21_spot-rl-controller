#!/bin/bash
# setup_jenkins.sh — Cài Jenkins LTS trên Ubuntu 22.04 (Digital Ocean Droplet)
# Usage: bash setup_jenkins.sh
# Chạy với root hoặc sudo

set -euo pipefail

echo "================================================"
echo " Jenkins Setup — Ubuntu 22.04"
echo "================================================"

# ── 1. System update ──────────────────────────────
echo "[1/6] Updating system..."
apt-get update -qq
apt-get upgrade -y -qq

# ── 2. Java 17 ───────────────────────────────────
echo "[2/6] Installing OpenJDK 17..."
apt-get install -y -qq openjdk-17-jdk curl wget gnupg2

java -version
echo "JAVA_HOME=$(dirname $(dirname $(readlink -f $(which java))))"
export JAVA_HOME=$(dirname $(dirname $(readlink -f $(which java))))

# ── 3. Jenkins LTS ───────────────────────────────
echo "[3/6] Installing Jenkins LTS..."
curl -fsSL https://pkg.jenkins.io/debian-stable/jenkins.io-2023.key \
    | gpg --dearmor -o /usr/share/keyrings/jenkins-keyring.gpg

echo "deb [signed-by=/usr/share/keyrings/jenkins-keyring.gpg] \
    https://pkg.jenkins.io/debian-stable binary/" \
    > /etc/apt/sources.list.d/jenkins.list

apt-get update -qq
apt-get install -y -qq jenkins

# ── 4. Tune JVM heap (2GB droplet: 1GB cho Jenkins) ──
echo "[4/6] Tuning JVM heap..."
mkdir -p /etc/systemd/system/jenkins.service.d
cat > /etc/systemd/system/jenkins.service.d/override.conf << 'EOF'
[Service]
Environment="JAVA_OPTS=-Xmx1g -Xms256m -XX:+UseG1GC"
EOF

systemctl daemon-reload

# ── 5. Start & enable Jenkins ─────────────────────
echo "[5/6] Starting Jenkins..."
systemctl enable jenkins
systemctl start jenkins

# Đợi Jenkins khởi động xong
echo "  Waiting for Jenkins to start..."
timeout 60 bash -c 'until systemctl is-active --quiet jenkins; do sleep 2; done'
sleep 10

# ── 6. Firewall ───────────────────────────────────
echo "[6/6] Configuring firewall..."
ufw allow OpenSSH
ufw allow 8080/tcp   # Jenkins UI
ufw allow 50000/tcp  # Jenkins agent JNLP port
ufw --force enable

# ── Done ──────────────────────────────────────────
DROPLET_IP=$(curl -s http://169.254.169.254/metadata/v1/interfaces/public/0/ipv4/address 2>/dev/null || hostname -I | awk '{print $1}')
INIT_PASSWORD=$(cat /var/lib/jenkins/secrets/initialAdminPassword 2>/dev/null || echo "Check: sudo cat /var/lib/jenkins/secrets/initialAdminPassword")

echo ""
echo "================================================"
echo " Jenkins is running!"
echo "================================================"
echo ""
echo "  URL:              http://${DROPLET_IP}:8080"
echo "  Initial password: ${INIT_PASSWORD}"
echo ""
echo "  Next steps:"
echo "  1. Truy cap http://${DROPLET_IP}:8080"
echo "  2. Nhap initial password de unlock"
echo "  3. Install suggested plugins"
echo "  4. Tao admin account"
echo ""
echo "  Jenkins agent port (JNLP): 50000"
echo "================================================"
