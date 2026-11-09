# A run block loading a module by its module_source from a `.tofutest.hcl`
# file: the manifest records it under tofu's key for that extension, which
# `tofu init -get=false` must find. Built, and so validated, for every tofu
# version rules_tofu pins (module_dependency_test.sh, step 1).
run "vpc" {
  command = plan

  module {
    source = "git::https://example.com/acme/network.git//vpc?ref=v1.0.0"
  }

  variables {
    name = "test"
  }

  assert {
    condition     = output.zone == "www.test.example.com"
    error_message = "zone was ${output.zone}"
  }
}
