run "greets" {
  command = plan

  assert {
    condition     = output.greeting == "Hello, bcr!"
    error_message = "greeting was ${output.greeting}"
  }
}
