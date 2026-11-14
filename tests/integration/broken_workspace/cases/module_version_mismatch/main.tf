# Deliberately incompatible with net's module_version = "1.0.0"; do not align versions.
module "net" {
  source  = "acme/network/null"
  version = "~> 2.0"
}
