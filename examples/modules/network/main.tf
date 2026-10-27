variable "cidr" {
  type = string
}

module "subnet" {
  source = "./subnet"
  cidr   = cidrsubnet(var.cidr, 8, 0)
}

output "subnets" {
  value = [module.subnet.cidr]
}
