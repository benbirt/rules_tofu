terraform {
  backend "local" {
    path = "${var.state_dir}/vars.tfstate"
  }
}

variable "state_dir" {
  type = string
}

variable "text" {
  type = string
}

variable "untyped" {}

variable "size" {
  type = number
}

variable "enabled" {
  type = bool
}

variable "zones" {
  type = list(string)
}

variable "tags" {
  type = map(string)
}

variable "anything" {
  type = any
}

variable "who" {
  type = string
}

variable "where" {
  type = string
}

variable "kept" {
  type = string
}

locals {
  got = {
    text     = var.text
    untyped  = var.untyped
    size     = var.size
    enabled  = var.enabled
    zones    = var.zones
    tags     = var.tags
    anything = var.anything
  }
  want = {
    text     = "it's \"quoted\", a=b, $HOME, `id` and $${braces}\nand a second line"
    untyped  = "3"
    size     = 3
    enabled  = true
    zones    = ["a", "b c"]
    tags     = { "a key" = "v=1", plain = "it's" }
    anything = { n = 3 }
  }
}

# Compare via jsonencode so string "3" and number 3 are distinct.
output "values" {
  value = "as written"

  precondition {
    condition     = jsonencode(local.got) == jsonencode(local.want)
    error_message = "vars arrived as ${jsonencode(local.got)}, want ${jsonencode(local.want)}"
  }
}

output "who" {
  value = var.who
}

output "where" {
  value = var.where
}

output "kept" {
  value = var.kept
}
