# The only place provider versions are pinned. `bazel run @tf_providers//:pin`
# (or Renovate/Dependabot) writes .terraform.lock.hcl beside this file, and
# MODULE.bazel's `tf_providers.from_lock` vendors what it pins.
terraform {
  required_providers {
    null = {
      source  = "hashicorp/null"
      version = "3.2.4"
    }
  }
}
