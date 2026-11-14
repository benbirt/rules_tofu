# This .tofutest.hcl file loads the same helper module as greet.tftest.hcl in a
# run block. That exercises recording a run module in modules.json under a
# .tofutest.hcl key, which OpenTofu formats differently from a .tftest.hcl key.
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
