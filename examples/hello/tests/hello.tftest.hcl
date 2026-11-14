# The hello deploy's vars bind name to "world", so this test sets nothing.
run "greets_the_world" {
  command = plan

  assert {
    condition     = output.greeting == "Hello, world!"
    error_message = "greeting was ${output.greeting}"
  }
}
