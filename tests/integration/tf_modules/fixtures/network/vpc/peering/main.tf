# A submodule outside modules/: part of the library only because //app calls
# it by `//vpc/peering`, so its provider is the caller's to pin and to carry.
terraform {
  required_providers {
    time = {
      source = "hashicorp/time"
    }
  }
}

variable "peer" {
  type = string
}

output "peer" {
  value = "${var.peer}-peered"
}
