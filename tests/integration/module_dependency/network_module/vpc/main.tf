variable "name" {
  type = string
}

module "dns" {
  source  = "acme/dns/null"
  version = "~> 1.2"
  zone    = "${var.name}.example.com"
}

output "zone" {
  value = module.dns.zone
}
