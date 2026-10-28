module "vpc" {
  source = "git::file://@FIXTURES@/network.git//vpc?ref=v1.0.0"
  name   = "app"
}

module "subnet" {
  source = "git::file://@FIXTURES@/network.git//vpc/modules/subnet?ref=v1.0.0"
  cidr   = "10.0.0.0/24"
}

module "dns" {
  source = "file://@FIXTURES@/dns.tar.gz"
  zone   = "example.com"
}

module "label" {
  source  = "cloudposse/label/null"
  version = "0.25.0"
  name    = "direct"
}

output "summary" {
  value = "${module.vpc.id} ${module.subnet.cidr} ${module.dns.fqdn} ${module.label.id}"
}
