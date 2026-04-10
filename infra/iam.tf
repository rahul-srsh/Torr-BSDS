# IAM role used by all ECS tasks (both task role and execution role).
#
# Execution role permissions: pull images from ECR, write CloudWatch logs.
# Task role permissions: write experiment results to S3, publish custom
# CloudWatch metrics, read/write ECR (for image pulls within the task).

resource "aws_iam_role" "ecs_task" {
  name = "hopvault-ecs-task-role"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect    = "Allow"
        Principal = { Service = "ecs-tasks.amazonaws.com" }
        Action    = "sts:AssumeRole"
      }
    ]
  })
}

# Standard ECS execution policy — lets ECS pull ECR images and write logs
resource "aws_iam_role_policy_attachment" "ecs_execution" {
  role       = aws_iam_role.ecs_task.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

# S3 access for experiment results bucket
resource "aws_iam_role_policy" "ecs_s3" {
  name = "hopvault-ecs-s3"
  role = aws_iam_role.ecs_task.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect = "Allow"
        Action = [
          "s3:PutObject",
          "s3:GetObject",
          "s3:ListBucket",
        ]
        Resource = [
          aws_s3_bucket.experiment_results.arn,
          "${aws_s3_bucket.experiment_results.arn}/*",
        ]
      }
    ]
  })
}

# CloudWatch custom metrics (for application-level instrumentation)
resource "aws_iam_role_policy" "ecs_cloudwatch" {
  name = "hopvault-ecs-cloudwatch"
  role = aws_iam_role.ecs_task.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect = "Allow"
        Action = [
          "cloudwatch:PutMetricData",
          "logs:CreateLogStream",
          "logs:PutLogEvents",
        ]
        Resource = "*"
      }
    ]
  })
}
