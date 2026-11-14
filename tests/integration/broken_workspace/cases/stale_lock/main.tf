terraform {
  required_providers {
    # Deliberately disagrees with 3.2.4 in //lock:.terraform.lock.hcl; do not bump.
    null = {
      source  = "hashicorp/null"
      version = "3.2.3"
    }
  }
}
