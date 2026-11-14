# Deliberately references an undeclared variable so `tofu validate` fails; do not declare it.
output "echo" {
  value = var.does_not_exist
}
