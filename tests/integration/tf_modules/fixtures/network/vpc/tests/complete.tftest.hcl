# The module's own tests, as upstream modules carry them. This one loads an
# example, whose configuration is no part of the generated library.
run "complete" {
  command = plan

  module {
    source = "./examples/complete"
  }
}
