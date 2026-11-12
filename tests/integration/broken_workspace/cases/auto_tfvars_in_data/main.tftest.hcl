run "foo" {
  command = plan

  assert {
    condition     = output.foo == "bar"
    error_message = "foo"
  }
}
