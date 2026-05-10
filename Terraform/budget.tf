# AWS Budget alert — layer ngoài cùng chống chi phí runaway.
# 3 thresholds: 50%, 80%, 100% của monthly budget → email alert.

resource "aws_sns_topic" "alerts" {
  name = "${var.project_name}-alerts"
}

resource "aws_sns_topic_subscription" "alerts_email" {
  topic_arn = aws_sns_topic.alerts.arn
  protocol  = "email"
  endpoint  = var.alert_email
  # NOTE: AWS gửi confirmation email — phải click link để activate.
}

resource "aws_budgets_budget" "monthly" {
  name         = "${var.project_name}-monthly-budget"
  budget_type  = "COST"
  limit_amount = tostring(var.monthly_budget_usd)
  limit_unit   = "USD"
  time_unit    = "MONTHLY"

  cost_filter {
    name = "TagKeyValue"
    values = [
      "user:Project$spot-rl-seminar",
    ]
  }

  # Alert ở 50% — early warning
  notification {
    comparison_operator        = "GREATER_THAN"
    threshold                  = 50
    threshold_type             = "PERCENTAGE"
    notification_type          = "ACTUAL"
    subscriber_email_addresses = [var.alert_email]
  }

  # Alert ở 80% — cần action
  notification {
    comparison_operator        = "GREATER_THAN"
    threshold                  = 80
    threshold_type             = "PERCENTAGE"
    notification_type          = "ACTUAL"
    subscriber_email_addresses = [var.alert_email]
  }

  # Alert ở 100% — KHẨN CẤP
  notification {
    comparison_operator        = "GREATER_THAN"
    threshold                  = 100
    threshold_type             = "PERCENTAGE"
    notification_type          = "ACTUAL"
    subscriber_email_addresses = [var.alert_email]
  }

  # Forecasted alert: AWS dự báo sẽ vượt budget
  notification {
    comparison_operator        = "GREATER_THAN"
    threshold                  = 100
    threshold_type             = "PERCENTAGE"
    notification_type          = "FORECASTED"
    subscriber_email_addresses = [var.alert_email]
  }
}

# CloudWatch alarm: alert khi có >5 EC2 instances tagged ManagedBy=spot-rl
# (defense-in-depth, độc lập với budget)
resource "aws_cloudwatch_metric_alarm" "fleet_size" {
  alarm_name          = "${var.project_name}-fleet-size-exceeded"
  alarm_description   = "Spot RL fleet vượt 15 instances — possible bug"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 2
  metric_name         = "FleetSize"
  namespace           = "SpotRL"
  period              = 300
  statistic           = "Maximum"
  threshold           = 15
  treat_missing_data  = "notBreaching"

  alarm_actions = [aws_sns_topic.alerts.arn]

  tags = {
    Name = "${var.project_name}-fleet-alarm"
  }
}
