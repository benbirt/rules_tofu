# Pins for the stale_lock case; .terraform.lock.hcl beside it is what
# `bazel run @tf_providers//:pin` writes for them.
terraform {
  required_providers {
    null = {
      source  = "hashicorp/null"
      version = "3.2.4"
    }
  }
}
