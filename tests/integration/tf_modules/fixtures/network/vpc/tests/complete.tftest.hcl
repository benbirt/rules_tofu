# Upstream test file in tests/, which tofu also reads by default; see
# ../defaults.tftest.hcl.
run "complete" {
  command = plan

  module {
    source = "./examples/complete"
  }
}
