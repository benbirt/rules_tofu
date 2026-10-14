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

  # The personal API key is read from POSTHOG_API_KEY at plan/apply time.
  # `tofu validate`, which rules_tofu runs at `bazel build` time, doesn't
  # contact PostHog.
}

resource "posthog_dashboard" "example" {
  name        = "rules_tofu example"
  description = "Managed by rules_tofu's examples/posthog."
}
