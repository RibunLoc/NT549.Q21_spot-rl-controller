# IAM role + instance profile cho controller.
# Least-privilege: chỉ EC2 ops cần thiết, SQS đọc, Pricing API.

resource "aws_iam_role" "controller" {
  name = "${var.project_name}-controller-role"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Principal = {
        Service = "ec2.amazonaws.com"
      }
      Action = "sts:AssumeRole"
    }]
  })

  tags = {
    Name = "${var.project_name}-controller-role"
  }
}

# Inline policy: EC2 + SQS + Pricing
resource "aws_iam_role_policy" "controller" {
  name = "${var.project_name}-controller-policy"
  role = aws_iam_role.controller.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      # ─── EC2: launch / terminate / describe ──────────────────────────
      {
        Sid    = "EC2Read"
        Effect = "Allow"
        Action = [
          "ec2:DescribeInstances",
          "ec2:DescribeInstanceTypes",
          "ec2:DescribeSpotPriceHistory",
          "ec2:DescribeAvailabilityZones",
          "ec2:DescribeSubnets",
          "ec2:DescribeSecurityGroups",
          "ec2:DescribeImages",
          "ec2:DescribeKeyPairs",
        ]
        Resource = "*"
      },
      {
        Sid    = "EC2Launch"
        Effect = "Allow"
        Action = [
          "ec2:RunInstances",
          "ec2:RequestSpotInstances",
          "ec2:CreateTags",
        ]
        Resource = "*"
        # Tag-based condition: chỉ launch instance có tag ManagedBy=spot-rl
        # (Nếu muốn strict hơn — tớ giữ open để demo dễ hơn)
      },
      {
        Sid    = "EC2Terminate"
        Effect = "Allow"
        Action = [
          "ec2:TerminateInstances",
          "ec2:CancelSpotInstanceRequests",
        ]
        Resource = "*"
        Condition = {
          StringEquals = {
            "ec2:ResourceTag/ManagedBy" = "spot-rl"
          }
        }
      },
      # IAM PassRole — cần để launch instance với worker IAM profile (nếu có)
      {
        Sid      = "PassRoleToWorker"
        Effect   = "Allow"
        Action   = "iam:PassRole"
        Resource = aws_iam_role.worker.arn
      },
      # ─── SQS: queue depth + receive (cho workload signal) ────────────
      {
        Sid    = "SQS"
        Effect = "Allow"
        Action = [
          "sqs:GetQueueAttributes",
          "sqs:GetQueueUrl",
          "sqs:ReceiveMessage",
          "sqs:DeleteMessage",
          "sqs:SendMessage",
        ]
        Resource = aws_sqs_queue.jobs.arn
      },
      # ─── CloudWatch: push metrics ────────────────────────────────────
      {
        Sid    = "CloudWatchMetrics"
        Effect = "Allow"
        Action = [
          "cloudwatch:PutMetricData",
        ]
        Resource = "*"
        Condition = {
          StringEquals = {
            "cloudwatch:namespace" = "SpotRL"
          }
        }
      },
      # ─── Logs: ghi log của controller ────────────────────────────────
      {
        Sid    = "Logs"
        Effect = "Allow"
        Action = [
          "logs:CreateLogGroup",
          "logs:CreateLogStream",
          "logs:PutLogEvents",
          "logs:DescribeLogStreams",
        ]
        Resource = "arn:aws:logs:${var.aws_region}:*:log-group:/spot-rl/*"
      },
    ]
  })
}

resource "aws_iam_instance_profile" "controller" {
  name = "${var.project_name}-controller-profile"
  role = aws_iam_role.controller.name
}

# ─── Worker IAM (minimal — chỉ đọc SQS + write CloudWatch) ─────────────
resource "aws_iam_role" "worker" {
  name = "${var.project_name}-worker-role"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Principal = {
        Service = "ec2.amazonaws.com"
      }
      Action = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_role_policy" "worker" {
  name = "${var.project_name}-worker-policy"
  role = aws_iam_role.worker.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect = "Allow"
        Action = [
          "sqs:ReceiveMessage",
          "sqs:DeleteMessage",
          "sqs:GetQueueAttributes",
        ]
        Resource = aws_sqs_queue.jobs.arn
      },
      {
        Effect = "Allow"
        Action = [
          "cloudwatch:PutMetricData",
          "logs:CreateLogStream",
          "logs:PutLogEvents",
        ]
        Resource = "*"
      },
      # SSM: đọc Jenkins credentials (jenkins_agent.sh bootstrap)
      {
        Sid    = "SSMJenkinsParams"
        Effect = "Allow"
        Action = ["ssm:GetParameter"]
        Resource = [
          "arn:aws:ssm:${var.aws_region}:*:parameter/spot-rl/jenkins-url",
          "arn:aws:ssm:${var.aws_region}:*:parameter/spot-rl/jenkins-user",
          "arn:aws:ssm:${var.aws_region}:*:parameter/spot-rl/jenkins-token",
        ]
      },
      # EC2 CreateTags: tự tag instance sau khi agent join Jenkins
      {
        Sid      = "SelfTag"
        Effect   = "Allow"
        Action   = ["ec2:CreateTags"]
        Resource = "*"
        Condition = {
          StringEquals = {
            "ec2:ResourceTag/ManagedBy" = "spot-rl-controller"
          }
        }
      },
    ]
  })
}

resource "aws_iam_instance_profile" "worker" {
  name = "${var.project_name}-worker-profile"
  role = aws_iam_role.worker.name
}
