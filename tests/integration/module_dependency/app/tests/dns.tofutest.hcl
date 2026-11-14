# A run block loading a module by its module_source from a `.tofutest.hcl`
# file: the manifest records it under tofu's key for that extension, which
# `tofu init -get=false` must find. Built, and so validated, and then run,
# for every tofu version rules_tofu pins (module_dependency_test.sh, steps 1
# and 5). The source is a registry address, which a run block's module has
# always accepted (older OpenTofu took only local and registry sources there),
# and dns_module's library provides this one.
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
