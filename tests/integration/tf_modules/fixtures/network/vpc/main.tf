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

resource "null_resource" "vpc" {
  triggers = {
    id = module.label.id
  }
}

output "id" {
  value = "${module.label.id}-${module.shared.suffix}"
}
