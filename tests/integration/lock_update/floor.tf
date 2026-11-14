terraform {
  required_providers {
    null = {
      source  = "hashicorp/null"
      version = ">= 3.0"
    }
  }
}

# A submodule of a bundle library in another Bazel module, whose own
# constraint must reach the lock too (issue #123).
module "bundle_sub" {
  source = "github.com/acme/bundle//sub"
}
