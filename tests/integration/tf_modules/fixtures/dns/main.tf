variable "zone" {
  type = string
}

output "fqdn" {
  value = "www.${var.zone}"
}
