# A run block loading a module by its module_source from a `.tofutest.hcl`
# file: the manifest records it under tofu's key for that extension, which
# `tofu init -get=false` must find. Built, and so validated, for every tofu
# version rules_tofu pins (module_dependency_test.sh, step 1). The source is
# a registry address: tofu 1.8 accepts only local and registry sources in a
# run block's module, and dns_module's library provides this one.
run "dns" {
  command = plan

  module {
    source  = "acme/dns/null"
    version = "~> 1.2"
  }

  variables {
    zone = "test.example.com"
  }

  assert {
    condition     = output.zone == "www.test.example.com"
    error_message = "zone was ${output.zone}"
  }
}
