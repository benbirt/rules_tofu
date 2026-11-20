terraform {
  required_providers {
    random = {
      source = "HashiCorp/random"
    }
    corp = {
      source = "example.com/acme/corp"
    }
    builtin = {
      source = "terraform.io/builtin/terraform"
    }
  }
}

provider "google" {
  alias = "eu"
}

provider "google" {
  alias = "us"
}

resource "null_resource" "implied" {}

data "http" "implied" {
  url = "https://example.com"
}
