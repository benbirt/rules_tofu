terraform {
  required_providers {
    posthog = {
      source  = "PostHog/posthog"
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

  # POSTHOG_API_KEY is read at plan and apply time. `bazel build` runs
  # `tofu validate` offline without contacting PostHog.
}

resource "posthog_dashboard" "example" {
  name        = "rules_tofu example"
  description = "Managed by rules_tofu's examples/posthog."
}
