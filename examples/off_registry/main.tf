terraform {
  required_providers {
    posthog = {
      # Off-registry provider pinned by tf_providers.provider in MODULE.bazel.
      source  = "example-corp/posthog"
      version = "1.0.24"
    }
  }
}

variable "project_id" {
  type        = string
  description = "PostHog project to manage."
}

provider "posthog" {
  host       = "https://eu.posthog.com"
  project_id = var.project_id
}

resource "posthog_dashboard" "example" {
  name        = "rules_tofu example"
  description = "Managed by rules_tofu's examples/off_registry."
}
