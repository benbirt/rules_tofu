# A submodule outside modules/ that the module never calls: part of the
# library only because the tag names it in `submodules`. The library then
# carries its provider.
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
