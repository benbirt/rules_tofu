# A submodule outside modules/ that the module itself calls, by its own
# address (vpc/main.tf): part of the library because of that call.
variable "name" {
  type = string
}

output "tags" {
  value = { Name = var.name }
}
