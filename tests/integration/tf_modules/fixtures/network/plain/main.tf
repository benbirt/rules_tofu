# Keep no other file in this directory. @tf_modules//plain checks a module
# whose library's data glob matches nothing (issue #164).
variable "name" {
  type = string
}

output "name" {
  value = "${var.name}-plain"
}
