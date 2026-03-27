resource "aws_service_discovery_private_dns_namespace" "hopvault" {
  name        = "hopvault.local"
  description = "Private DNS namespace for HopVault service discovery"
  vpc         = aws_vpc.hopvault.id
}

resource "aws_service_discovery_service" "services" {
  for_each = local.service_definitions

  name = each.key

  dns_config {
    namespace_id   = aws_service_discovery_private_dns_namespace.hopvault.id
    routing_policy = "MULTIVALUE"

    dns_records {
      ttl  = 10
      type = "A"
    }
  }

  health_check_custom_config {
    failure_threshold = 1
  }
}
