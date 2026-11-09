terraform {
  backend "local" {
    path = "state/dev.tfstate"
  }
}

output "echo" {
  value = "hello"
}
