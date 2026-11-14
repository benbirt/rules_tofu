# Calls an address with no matching dep in BUILD.bazel; keep deps omitted.
module "net" { source = "git::https://example.com/acme/network.git?ref=v1.0.0" }
