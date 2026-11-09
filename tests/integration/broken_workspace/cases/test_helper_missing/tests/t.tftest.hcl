run "setup" {
  module {
    source = "./helper"
  }
}

run "x" {
  command = plan

  assert {
    condition     = output.x == 1
    error_message = "x"
  }
}
