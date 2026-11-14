# Calls an undeclared module so fetching @tf_modules//caller fails (tf_modules_test.sh, step 7).
module "missing" {
  source  = "acme/missing/null"
  version = "1.0.0"
}
