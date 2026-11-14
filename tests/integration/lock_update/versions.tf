# Locked at 3.2.3 first and bumped to 3.2.4 by lock_update_test.sh; keep 3.2.3.
terraform {
  required_providers {
    null = {
      source  = "hashicorp/null"
      version = "3.2.3"
    }
  }
}
