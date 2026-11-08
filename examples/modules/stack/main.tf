module "stack" {
  source  = "cloudposse/stack-config/yaml//modules/stack"
  version = "~> 2.0"

  tenant      = "acme"
  environment = "ue2"
  stage       = "prod"
}

output "stack_name" {
  value = module.stack.stack_name
}
