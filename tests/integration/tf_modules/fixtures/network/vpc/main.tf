terraform {
  required_providers {
    null = {
      source = "hashicorp/null"
    }
  }
}

variable "name" {
  type = string
}

module "label" {
  source  = "cloudposse/label/null"
  version = "~> 0.25.0"
  name    = var.name
}

module "shared" {
  source = "../shared"
}

# By the module's own address, as some modules call their submodules: the
# test driver fills in @FIXTURES@ before it commits this file.
module "tags" {
  source = "git::file://@FIXTURES@/network.git//vpc/tags?ref=v1.0.0"
  name   = var.name
}

resource "null_resource" "vpc" {
  triggers = {
    id   = module.label.id
    name = module.tags.tags.Name
  }
}

output "id" {
  value = "${module.label.id}-${module.shared.suffix}"
}
