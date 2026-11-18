# A test file in the module's own directory, which tofu reads without being
# told. It loads an example, whose configuration is no part of the generated
# library.
run "defaults" {
  command = plan

  module {
    source = "./examples/complete"
  }
}
