resource "aws_security_group" "services" {
  name        = "hopvault-services-sg"
  description = "Allow all traffic between HopVault services"
  vpc_id      = aws_vpc.hopvault.id

  egress {
    from_port        = 0
    to_port          = 0
    protocol         = "-1"
    cidr_blocks      = ["0.0.0.0/0"]
    ipv6_cidr_blocks = ["::/0"]
  }
}

resource "aws_vpc_security_group_ingress_rule" "services_tcp_self" {
  security_group_id            = aws_security_group.services.id
  referenced_security_group_id = aws_security_group.services.id
  ip_protocol                  = "tcp"
  from_port                    = 0
  to_port                      = 65535
  description                  = "Allow inter-service TCP traffic"
}

# Allow inbound on 8080 from anywhere (for directory server public access)
resource "aws_vpc_security_group_ingress_rule" "services_public_8080" {
  security_group_id = aws_security_group.services.id
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "tcp"
  from_port         = 8080
  to_port           = 8080
  description       = "Allow public access to service port"
}

resource "aws_ecs_task_definition" "services" {
  for_each = local.service_definitions

  family                   = "hopvault-${replace(each.key, "-", "")}"
  network_mode             = "awsvpc"
  requires_compatibilities = ["FARGATE"]
  cpu                      = "256"
  memory                   = "512"
  task_role_arn            = data.aws_iam_role.lab_role.arn
  execution_role_arn       = data.aws_iam_role.lab_role.arn

  container_definitions = jsonencode([
    {
      name      = each.key
      image     = "${aws_ecr_repository.services[each.key].repository_url}:latest"
      essential = true
      environment = [
        {
          name  = "NODE_TYPE"
          value = each.key
        },
        {
          name  = "PORT"
          value = "8080"
        },
        {
          name  = "DIRECTORY_SERVER_URL"
          value = "http://directory-server.hopvault.local:8080"
        },
        {
          name  = "EXPERIMENT_RESULTS_BUCKET"
          value = aws_s3_bucket.experiment_results.bucket
        },
      ]
      portMappings = [
        {
          containerPort = 8080
          protocol      = "tcp"
        },
      ]
      logConfiguration = {
        logDriver = "awslogs"
        options = {
          awslogs-group         = aws_cloudwatch_log_group.services[each.key].name
          awslogs-region        = data.aws_region.current.name
          awslogs-stream-prefix = each.key
        }
      }
      healthCheck = {
        command     = ["CMD-SHELL", "wget -qO- http://localhost:8080/health || exit 1"]
        interval    = 10
        timeout     = 5
        startPeriod = 10
        retries     = 3
      }
    }
  ])
}

resource "aws_ecs_service" "services" {
  for_each = local.service_definitions

  name                               = each.key
  cluster                            = aws_ecs_cluster.hopvault.id
  task_definition                    = aws_ecs_task_definition.services[each.key].arn
  desired_count                      = each.value.desired_count
  launch_type                        = "FARGATE"
  deployment_minimum_healthy_percent = 50
  deployment_maximum_percent         = 200

  network_configuration {
    subnets = each.value.public ? [
      aws_subnet.public_az1.id, aws_subnet.public_az2.id
      ] : [
      aws_subnet.private_az1.id, aws_subnet.private_az2.id
    ]
    security_groups  = [aws_security_group.services.id]
    assign_public_ip = each.value.public
  }

  depends_on = [
    aws_cloudwatch_log_group.services,
  ]
}
