# The subject's name comes from a helper module, as a fixture would. Its
# source is relative to the module under test's directory, examples/hello,
# and the module is a tf_library in the test's deps.
run "subject" {
  module {
    source = "./tests/subject"
  }
}

run "greets_the_subject" {
  command = plan

  variables {
    name = run.subject.name
  }

  assert {
    condition     = output.greeting == "Hello, Tester!"
    error_message = "greeting was ${output.greeting}"
  }
}
