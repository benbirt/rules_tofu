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

# Calls a submodule by the module's own address, built from a local value and
# a variable default so the fetched-module scan exercises OpenTofu evaluation.
# The test driver fills in @FIXTURES@ before committing this file.
locals {
  repository = "git::file://@FIXTURES@/network.git"
}

variable "ref" {
  type    = string
  default = "v1.0.0"
}

module "tags" {
  source = "${local.repository}//vpc/tags?ref=${var.ref}"
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
