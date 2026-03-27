resource "aws_cloudwatch_log_group" "services" {
  for_each = local.service_definitions

  name              = "/hopvault/${each.key}"
  retention_in_days = 7
}

resource "aws_cloudwatch_dashboard" "hopvault" {
  dashboard_name = "HopVault-Overview"

  dashboard_body = jsonencode({
    widgets = [
      {
        type   = "metric"
        x      = 0
        y      = 0
        width  = 12
        height = 6
        properties = {
          view   = "timeSeries"
          region = data.aws_region.current.name
          title  = "CPU Utilization by Service"
          metrics = [
            for service_name in local.service_names :
            ["AWS/ECS", "CPUUtilization", "ClusterName", aws_ecs_cluster.hopvault.name, "ServiceName", service_name, { label = service_name, period = 60 }]
          ]
        }
      },
      {
        type   = "metric"
        x      = 12
        y      = 0
        width  = 12
        height = 6
        properties = {
          view   = "timeSeries"
          region = data.aws_region.current.name
          title  = "Memory Utilization by Service"
          metrics = [
            for service_name in local.service_names :
            ["AWS/ECS", "MemoryUtilization", "ClusterName", aws_ecs_cluster.hopvault.name, "ServiceName", service_name, { label = service_name, period = 60 }]
          ]
        }
      },
      {
        type   = "metric"
        x      = 0
        y      = 6
        width  = 12
        height = 6
        properties = {
          view   = "timeSeries"
          region = data.aws_region.current.name
          title  = "Running Task Count by Service"
          metrics = [
            for service_name in local.service_names :
            ["ECS/ContainerInsights", "RunningTaskCount", "ClusterName", aws_ecs_cluster.hopvault.name, "ServiceName", service_name, { label = service_name, period = 60 }]
          ]
        }
      },
      {
        type   = "log"
        x      = 12
        y      = 6
        width  = 12
        height = 6
        properties = {
          region = data.aws_region.current.name
          title  = "Recent Errors (all services)"
          view   = "table"
          query = join(
            " ",
            concat(
              [for service_name in local.service_names : "SOURCE '/hopvault/${service_name}' |"],
              [
                "fields @timestamp, @logStream, @message",
                "| filter @message like /(?i)(error|fatal|panic)/",
                "| sort @timestamp desc",
                "| limit 50",
              ]
            )
          )
        }
      },
    ]
  })
}
