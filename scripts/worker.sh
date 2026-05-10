#!/bin/bash
# Worker script chạy trên EC2 Spot Instance
# Tự động poll SQS và xử lý jobs

set -e

# === Config (được inject qua EC2 User Data) ===
QUEUE_URL="${SQS_QUEUE_URL}"
REGION="${AWS_DEFAULT_REGION:-ap-southeast-1}"
LOG_FILE="/var/log/worker.log"

log() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] $1" | tee -a "$LOG_FILE"
}

log "=== Worker starting ==="
log "Queue: $QUEUE_URL"
log "Region: $REGION"

# === Main loop ===
while true; do
    # 1. Poll message từ SQS (wait tối đa 20s nếu queue trống)
    RESPONSE=$(aws sqs receive-message \
        --queue-url "$QUEUE_URL" \
        --region "$REGION" \
        --max-number-of-messages 1 \
        --wait-time-seconds 20 \
        --output json 2>/dev/null)

    # 2. Kiểm tra có message không
    BODY=$(echo "$RESPONSE" | python3 -c "
import sys, json
data = json.load(sys.stdin)
msgs = data.get('Messages', [])
if msgs:
    print(msgs[0]['Body'])
    print(msgs[0]['ReceiptHandle'])
" 2>/dev/null)

    if [ -z "$BODY" ]; then
        log "Queue empty, waiting..."
        sleep 5
        continue
    fi

    # Parse body và receipt handle
    JOB_BODY=$(echo "$BODY" | head -1)
    RECEIPT=$(echo "$BODY" | tail -1)
    JOB_ID=$(echo "$JOB_BODY" | python3 -c "
import sys, json
print(json.load(sys.stdin).get('job_id', 'unknown'))
" 2>/dev/null)

    log "Processing job_id=$JOB_ID"

    # 3. Xử lý job — thay đoạn này bằng logic thật
    sleep $((RANDOM % 10 + 5))   # giả lập xử lý 5-15 giây
    log "Job $JOB_ID done"

    # 4. Xóa message khỏi SQS — báo đã xử lý xong
    aws sqs delete-message \
        --queue-url "$QUEUE_URL" \
        --receipt-handle "$RECEIPT" \
        --region "$REGION"

    log "Message deleted from queue"
done
