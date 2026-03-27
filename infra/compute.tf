resource "aws_ecs_cluster" "hopvault" {
  name = "hopvault-cluster"
}

resource "aws_ecr_repository" "services" {
  for_each = toset([
    "directory-server",
    "guard-node",
    "relay-node",
    "exit-node",
    "echo-server",
  ])

  name         = "hopvault/${each.value}"
  force_delete = true
}
