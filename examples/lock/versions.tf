# This file pins exact provider versions for the lock example.
# `bazel run @tf_providers//:pin`, Renovate or Dependabot updates
# .terraform.lock.hcl beside it. MODULE.bazel reads that lock with
# tf_providers.from_lock.
terraform {
  required_providers {
    null = {
      source  = "hashicorp/null"
      version = "3.3.2"
    }
  }
}
