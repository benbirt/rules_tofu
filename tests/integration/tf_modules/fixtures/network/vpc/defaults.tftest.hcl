# Upstream test file in the module root, loading an excluded example.
# tf_modules must omit test files from data so validating @tf_modules//network
# does not read this file (tf_modules_test.sh, step 3).
run "defaults" {
  command = plan

  module {
    source = "./examples/complete"
  }
}
