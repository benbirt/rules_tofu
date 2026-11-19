run "values" {
  command = plan

  assert {
    condition     = output.values == "as written"
    error_message = "the deploy's vars did not reach tofu test"
  }

  assert {
    condition     = output.who == "test file"
    error_message = "who = ${output.who}: the test's var file did not override the deploy's vars"
  }

  assert {
    condition     = output.where == "test file"
    error_message = "where = ${output.where}: the test's var file did not override the deploy's var file"
  }

  assert {
    condition     = output.kept == "deploy file"
    error_message = "kept = ${output.kept}: the deploy's var file did not reach tofu test"
  }
}
