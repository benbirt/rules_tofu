# OpenTofu reads this file in place of main.tf.
output "greeting" {
  value = "Hello from OpenTofu, ${var.name}!"
}
