variable "zone" {
  type = string
}

output "fqdn" {
  value = "www.${var.zone}"
}

# Neither resource has a required_providers entry. OpenTofu implies
# hashicorp/tls for tls_private_key, which the generated library lists in
# `providers` and pin must lock. OpenTofu also implies the built-in provider
# for terraform_data, which is dropped. Nothing else in the workspace requires
# hashicorp/tls.
resource "tls_private_key" "zone" {
  algorithm = "ED25519"
}

resource "terraform_data" "zone" {}
