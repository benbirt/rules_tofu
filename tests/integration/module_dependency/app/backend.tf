variable "state_root" {
  type = string
}

# Overridden by backend_override.tf; kept relative so an unmerged read fails.
terraform {
  backend "local" {
    path = "unused/app.tfstate"
  }
}
