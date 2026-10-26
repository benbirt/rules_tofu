terraform {
  required_providers {
    # The lock pins 3.2.4, as if this pin were bumped without re-pinning.
    null = {
      source  = "hashicorp/null"
      version = "3.2.3"
    }
  }
}
