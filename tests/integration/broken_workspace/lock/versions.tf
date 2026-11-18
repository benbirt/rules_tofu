# Exact pins for broken_workspace's .terraform.lock.hcl: hashicorp/null for
# stale_lock, and hashicorp/random for cases needing a valid pinned provider.
terraform {
  required_providers {
    null = {
      source  = "hashicorp/null"
      version = "3.2.4"
    }
    random = {
      source  = "hashicorp/random"
      version = "3.9.1"
    }
  }
}
