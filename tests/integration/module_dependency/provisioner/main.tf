resource "terraform_data" "hello" {
  provisioner "local-exec" {
    command = "${path.module}/hello.sh"
  }
}

output "greeting" {
  value = file("${path.module}/greeting.txt")
}
