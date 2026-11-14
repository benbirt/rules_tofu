# This run block loads a helper module supplied by a tf_library in deps. Its
# source path is resolved relative to the module under test's directory,
# examples/hello, rather than this test directory.
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
