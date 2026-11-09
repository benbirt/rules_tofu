variable "foo" {
  type    = string
  default = "bar"
}

output "foo" {
  value = var.foo
}
