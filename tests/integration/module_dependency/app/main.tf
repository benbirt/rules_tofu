module "vpc" {
  source = "git::https://example.com/acme/network.git//vpc?ref=v1.0.0"
  name   = "app"
}

output "zone" {
  value = module.vpc.zone
}
