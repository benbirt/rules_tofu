variable "child" {
  type    = string
  default = "./child"
}

locals {
  child = "./child"
}

module "from_variable" {
  source = var.child
}

module "from_local" {
  source = local.child
}

module "overridden" {
  source = "./absent"
}

output "names" {
  value = [
    module.from_variable.name,
    module.from_local.name,
    module.overridden.name,
    module.from_tofu.name,
    module.from_json.name,
  ]
}
