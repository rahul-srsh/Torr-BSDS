variable "directory_server_url" {
  description = "URL of the directory server used by nodes for self-registration. Set after the directory server's public IP is known. Pass as -var or TF_VAR_directory_server_url."
  type        = string
  default     = ""
}

data "aws_caller_identity" "current" {}

data "aws_region" "current" {}

data "aws_availability_zones" "available" {
  state = "available"
}

locals {
  name_prefix = "hopvault"

  service_definitions = {
    directory-server = { desired_count = 1, public = true, node_type = "directory-server" }
    guard-node       = { desired_count = 1, public = true, node_type = "guard" }
    relay-node       = { desired_count = 2, public = true, node_type = "relay" }
    exit-node        = { desired_count = 1, public = true, node_type = "exit" }
    echo-server      = { desired_count = 1, public = true, node_type = "echo-server" }
  }

  service_names = keys(local.service_definitions)

  private_subnet_cidrs = {
    private_az1 = "10.0.2.0/24"
    private_az2 = "10.0.3.0/24"
  }

  public_subnet_cidrs = {
    public_az1 = "10.0.0.0/24"
    public_az2 = "10.0.1.0/24"
  }

  experiment_results_bucket_name = format(
    "%s-experiment-results-%s-%s",
    local.name_prefix,
    data.aws_caller_identity.current.account_id,
    data.aws_region.current.name
  )
}
