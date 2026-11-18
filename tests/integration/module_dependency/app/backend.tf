variable "state_root" {
  type = string
}

# Overridden by backend_override.tf, so this relative path is never used.
terraform {
  backend "local" {
    path = "unused/app.tfstate"
  }
}
