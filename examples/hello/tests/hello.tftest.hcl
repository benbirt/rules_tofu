run "greets_the_world" {
  command = plan

  assert {
    condition     = output.greeting == "Hello, world!"
    error_message = "greeting was ${output.greeting}"
  }
}
