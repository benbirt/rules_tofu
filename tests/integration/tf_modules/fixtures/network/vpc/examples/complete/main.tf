# Not part of the module's library, so its provider isn't the caller's to pin.
terraform {
  required_providers {
    random = {
      source = "hashicorp/random"
    }
  }
}

module "vpc" {
  source = "../.."
  name   = "example"
}
