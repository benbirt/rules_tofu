# Terraform reads this file. OpenTofu ignores it because main.tofu is present.
output "greeting" {
  value = "Hello, ${var.name}!"
}
