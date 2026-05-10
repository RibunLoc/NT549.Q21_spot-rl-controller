# SQS queue: workload jobs.
# Producer: workload generator script (Python — push job mỗi N giây).
# Consumer: worker nodes (đọc job → xử lý → xóa message).
# Controller: query queue depth (làm pending_jobs feature).

resource "aws_sqs_queue" "jobs" {
  name                       = "${var.project_name}-jobs"
  visibility_timeout_seconds = 300  # 5 phút — đủ cho worker process
  message_retention_seconds  = 3600 # 1h — job cũ tự drop
  receive_wait_time_seconds  = 20   # long polling

  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.jobs_dlq.arn
    maxReceiveCount     = 3
  })

  tags = {
    Name = "${var.project_name}-jobs"
  }
}

# Dead letter queue — message fail 3 lần sẽ vào đây (để debug)
resource "aws_sqs_queue" "jobs_dlq" {
  name                      = "${var.project_name}-jobs-dlq"
  message_retention_seconds = 86400 # 1 ngày

  tags = {
    Name = "${var.project_name}-jobs-dlq"
  }
}
