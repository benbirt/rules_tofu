terraform {
  backend "local" {
    path = "state/dev.tfstate"
  }
}

# A resource of the provider built into tofu, so applying needs no plugin and
# creates nothing outside the state.
resource "terraform_data" "greeting" {
  input = trimspace(file("${path.module}/greeting.txt"))
}

output "greeting" {
  value = terraform_data.greeting.output
}
