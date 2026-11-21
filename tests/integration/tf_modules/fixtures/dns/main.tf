variable "zone" {
  type = string
}

output "fqdn" {
  value = "www.${var.zone}"
}

# Neither resource has a required_providers entry. OpenTofu implies
# hashicorp/null for null_resource, which the generated library must list in
# `providers`, and the built-in provider for terraform_data, which is dropped.
resource "null_resource" "zone" {}

resource "terraform_data" "zone" {}
