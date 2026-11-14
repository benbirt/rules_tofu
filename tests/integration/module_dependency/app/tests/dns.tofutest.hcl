# Loads @dns_module//:dns (acme/dns/null) by module_source from a .tofutest.hcl
# run block so `tofu init -get=false` checks the manifest key tofu uses for
# that extension. Validated and run on every pinned tofu version
# (module_dependency_test.sh, steps 1 and 5). Keep source as a registry
# address: older OpenTofu releases accepted only local and registry sources in
# run blocks.
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
