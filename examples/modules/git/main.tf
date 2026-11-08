module "label" {
  source = "git::https://github.com/cloudposse/terraform-null-label.git?ref=0.25.0"

  namespace = "acme"
  stage     = "prod"
  name      = "git"
}

output "id" {
  value = module.label.id
}
