# Deliberately uses a relative local backend path for local_backend_refused; keep it relative.
terraform {
  backend "local" {
    path = "state/dev.tfstate"
  }
}

output "echo" {
  value = "hello"
}
