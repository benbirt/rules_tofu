"""Rules for OpenTofu and Terraform-compatible configuration.

`tf_library` bundles reusable `.tf` files; `tf_deploy` is a root
configuration you can plan, apply and destroy with `bazel run`; `tf_test`
runs a module's `.tftest.hcl` tests with `bazel test`. Load them with:

    load("@rules_tofu//tf:defs.bzl", "tf_library", "tf_deploy", "tf_test")
"""

load("//tf/private:deploy.bzl", _tf_deploy = "tf_deploy")
load("//tf/private:library.bzl", _tf_library = "tf_library")
load("//tf/private:test.bzl", _tf_test = "tf_test")

tf_library = _tf_library
tf_deploy = _tf_deploy
tf_test = _tf_test
