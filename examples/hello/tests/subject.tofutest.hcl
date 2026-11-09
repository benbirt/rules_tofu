# A `.tofutest.hcl` file is OpenTofu's own test extension, read like a
# `.tftest.hcl`. Its `run` block loads the same helper module as
# greet.tftest.hcl, so the module manifest records a run module under a
# `.tofutest.hcl` key, which tofu writes differently from a `.tftest.hcl` one.
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
