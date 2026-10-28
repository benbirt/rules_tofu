module "label" {
  source  = "cloudposse/label/null"
  version = "~> 0.25.0"

  namespace = "acme"
  stage     = "prod"
  name      = "app"
}

output "id" {
  value = module.label.id
}
