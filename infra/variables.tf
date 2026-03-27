data "aws_caller_identity" "current" {}

data "aws_region" "current" {}

data "aws_availability_zones" "available" {
  state = "available"
}

data "aws_iam_role" "lab_role" {
  name = "LabRole"
}

locals {
  name_prefix = "hopvault"

  service_definitions = {
    directory-server = {
      desired_count = 1
    }
    guard-node = {
      desired_count = 1
    }
    relay-node = {
      desired_count = 2
    }
    exit-node = {
      desired_count = 1
    }
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
