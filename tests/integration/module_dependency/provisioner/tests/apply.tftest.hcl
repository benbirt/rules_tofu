run "apply" {
  assert {
    condition     = output.greeting == "hello\n"
    error_message = "greeting.txt was not read"
  }
  assert {
    condition     = file("${path.module}/ran.txt") == "hello\n"
    error_message = "hello.sh did not write ran.txt beside itself"
  }
}
