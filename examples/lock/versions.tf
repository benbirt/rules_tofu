# Exact pins for every registry provider used under examples/.
# `bazel run @tf_providers//:pin`, Renovate or Dependabot updates
# .terraform.lock.hcl beside it. MODULE.bazel reads that lock with
# tf_providers.from_lock.
#
# Dependabot's opentofu updater reads only this directory and would drop any
# provider required solely elsewhere. That includes aws, google and posthog
# (which examples/aws, examples/gcp and examples/posthog constrain with `>=`
# floors) as well as cloudposse/utils, hashicorp/external and hashicorp/local
# (required only by the fetched module in examples/modules/stack).
terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "6.73.0"
    }
    external = {
      source  = "hashicorp/external"
      version = "2.4.2"
    }
    google = {
      source  = "hashicorp/google"
      version = "8.11.0"
    }
    local = {
      source  = "hashicorp/local"
      version = "2.9.1"
    }
    null = {
      source  = "hashicorp/null"
      version = "3.3.2"
    }
    posthog = {
      source  = "PostHog/posthog"
      version = "1.0.31"
    }
    utils = {
      source  = "cloudposse/utils"
      version = "2.7.0"
    }
  }
}
