# Terraform reads this file; OpenTofu reads main.tofu instead.
output "greeting" {
  value = "Hello, ${var.name}!"
}
