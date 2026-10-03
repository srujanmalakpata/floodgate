resource "aws_cloudwatch_log_group" "gateway" {
  name              = "/ecs/${var.name}-gateway"
  retention_in_days = var.log_retention_days
}

locals {
  gateway_config = yamlencode({
    listen       = ":8080"
    admin_listen = ":9090"
    upstream     = var.upstream_url
    log_level    = "info"
    backend = {
      type = "redis"
      redis = {
        addr         = "${aws_elasticache_replication_group.this.primary_endpoint_address}:6379"
        key_prefix   = var.name
        timeout      = "50ms"
        password_env = "REDIS_PASSWORD"
        tls          = true
      }
      failure_policy = var.failure_policy
      breaker        = { failure_threshold = 5, cooldown = "5s" }
    }
    key      = { header = "X-API-Key", trusted_proxy_hops = 1 } # the ALB appends X-Forwarded-For
    routes   = var.routes
    reload   = { interval = "0s" } # config changes ship as a new task definition
    shutdown = { drain_delay = "5s", timeout = "15s" }
  })
}

resource "aws_ecs_cluster" "this" {
  name = var.name
  setting {
    name  = "containerInsights"
    value = "enabled"
  }
}

data "aws_iam_policy_document" "ecs_tasks_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }
  }
}

# Execution role: used by the ECS agent to pull the image, write logs and
# inject the Redis secret. The gateway itself needs no AWS permissions.
resource "aws_iam_role" "execution" {
  name_prefix        = "${var.name}-exec-"
  assume_role_policy = data.aws_iam_policy_document.ecs_tasks_assume.json
}

resource "aws_iam_role_policy_attachment" "execution" {
  role       = aws_iam_role.execution.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

data "aws_iam_policy_document" "read_redis_secret" {
  statement {
    actions   = ["secretsmanager:GetSecretValue"]
    resources = [aws_secretsmanager_secret.redis_auth.arn]
  }
}

resource "aws_iam_role_policy" "read_redis_secret" {
  name   = "read-redis-auth"
  role   = aws_iam_role.execution.id
  policy = data.aws_iam_policy_document.read_redis_secret.json
}

resource "aws_ecs_task_definition" "gateway" {
  family                   = "${var.name}-gateway"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = var.task_cpu
  memory                   = var.task_memory
  execution_role_arn       = aws_iam_role.execution.arn

  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = "X86_64"
  }

  # ECS has no ConfigMap: a short-lived init container writes the rendered
  # config into a task-scoped volume that the gateway mounts read-only.
  volume {
    name = "config"
  }

  container_definitions = jsonencode([
    {
      name      = "config-init"
      image     = "public.ecr.aws/docker/library/busybox:1.37"
      essential = false
      command   = ["sh", "-c", "echo \"$CONFIG_B64\" | base64 -d > /etc/rlgw/config.yaml"]
      environment = [
        { name = "CONFIG_B64", value = base64encode(local.gateway_config) },
      ]
      mountPoints = [{ sourceVolume = "config", containerPath = "/etc/rlgw" }]
      logConfiguration = {
        logDriver = "awslogs"
        options = {
          awslogs-group         = aws_cloudwatch_log_group.gateway.name
          awslogs-region        = var.aws_region
          awslogs-stream-prefix = "init"
        }
      }
    },
    {
      name      = "gateway"
      image     = var.image
      essential = true
      command   = ["-config", "/etc/rlgw/config.yaml"]
      dependsOn = [{ containerName = "config-init", condition = "SUCCESS" }]
      portMappings = [
        { containerPort = 8080, protocol = "tcp" },
        { containerPort = 9090, protocol = "tcp" },
      ]
      secrets = [
        { name = "REDIS_PASSWORD", valueFrom = aws_secretsmanager_secret.redis_auth.arn },
      ]
      mountPoints            = [{ sourceVolume = "config", containerPath = "/etc/rlgw", readOnly = true }]
      readonlyRootFilesystem = true
      user                   = "65532:65532"
      stopTimeout            = 30 # > drain_delay + shutdown timeout
      healthCheck = {
        command     = ["CMD", "/usr/local/bin/gateway", "-probe", "http://127.0.0.1:9090/healthz"]
        interval    = 10
        timeout     = 3
        retries     = 3
        startPeriod = 5
      }
      logConfiguration = {
        logDriver = "awslogs"
        options = {
          awslogs-group         = aws_cloudwatch_log_group.gateway.name
          awslogs-region        = var.aws_region
          awslogs-stream-prefix = "gateway"
        }
      }
    },
  ])
}

resource "aws_ecs_service" "gateway" {
  name            = "${var.name}-gateway"
  cluster         = aws_ecs_cluster.this.id
  task_definition = aws_ecs_task_definition.gateway.arn
  desired_count   = var.desired_count
  launch_type     = "FARGATE"

  deployment_minimum_healthy_percent = 100
  deployment_maximum_percent         = 200
  health_check_grace_period_seconds  = 15

  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }

  network_configuration {
    subnets          = aws_subnet.private[*].id
    security_groups  = [aws_security_group.tasks.id]
    assign_public_ip = false
  }

  load_balancer {
    target_group_arn = aws_lb_target_group.gateway.arn
    container_name   = "gateway"
    container_port   = 8080
  }

  # Autoscaling owns the task count after creation.
  lifecycle {
    ignore_changes = [desired_count]
  }

  depends_on = [aws_lb_listener.http]
}

resource "aws_appautoscaling_target" "gateway" {
  service_namespace  = "ecs"
  resource_id        = "service/${aws_ecs_cluster.this.name}/${aws_ecs_service.gateway.name}"
  scalable_dimension = "ecs:service:DesiredCount"
  min_capacity       = var.desired_count
  max_capacity       = var.max_count
}

resource "aws_appautoscaling_policy" "cpu" {
  name               = "${var.name}-cpu-target"
  policy_type        = "TargetTrackingScaling"
  service_namespace  = aws_appautoscaling_target.gateway.service_namespace
  resource_id        = aws_appautoscaling_target.gateway.resource_id
  scalable_dimension = aws_appautoscaling_target.gateway.scalable_dimension

  target_tracking_scaling_policy_configuration {
    target_value = 60
    predefined_metric_specification {
      predefined_metric_type = "ECSServiceAverageCPUUtilization"
    }
  }
}
