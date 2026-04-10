# Relay node auto-scaling configuration.
#
# min=2, max=20. Scaling policies are intentionally NOT attached here —
# desired count is managed manually during experiments via:
#
#   make scale-relays COUNT=N
#
# which calls `aws ecs update-service --desired-count N` directly and
# waits until all tasks are running and registered with the directory server.

resource "aws_appautoscaling_target" "relay" {
  service_namespace  = "ecs"
  resource_id        = "service/${aws_ecs_cluster.hopvault.name}/${aws_ecs_service.services["relay-node"].name}"
  scalable_dimension = "ecs:service:DesiredCount"
  min_capacity       = 2
  max_capacity       = 20

  depends_on = [aws_ecs_service.services]
}
