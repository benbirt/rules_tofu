variable "zone" {
  type = string
}

module "record" {
  source = "./record"
  name   = "www.${var.zone}"
}

output "zone" {
  value = module.record.name
}
