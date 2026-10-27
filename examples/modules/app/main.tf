module "network" {
  source = "git::https://example.com/acme/network.git?ref=v1.0.0"
  cidr   = "10.0.0.0/16"
}

module "extra_subnet" {
  source = "git::https://example.com/acme/network.git//subnet?ref=v1.0.0"
  cidr   = "10.1.0.0/24"
}

output "subnets" {
  value = concat(module.network.subnets, [module.extra_subnet.cidr])
}
