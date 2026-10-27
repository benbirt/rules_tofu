terraform {
  required_providers {
    # A reusable module names the provider and, optionally, the oldest version
    # it works with. The exact version comes from ../versions.tf and the lock.
    null = {
      source  = "hashicorp/null"
      version = ">= 3.0"
    }
  }
}

variable "name" {
  type    = string
  default = "world"
}

resource "null_resource" "greeting" {
  triggers = {
    greeting = "Hello, ${var.name}!"
  }
}
