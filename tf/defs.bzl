"""Rules for OpenTofu and Terraform-compatible configuration.

`tf_library` bundles reusable `.tf` files; `tf_deploy` is a root
configuration you can plan, apply and destroy with `bazel run`. Load them
with:

    load("@rules_tofu//tf:defs.bzl", "tf_library", "tf_deploy")
"""

load("//tf/private:deploy.bzl", _tf_deploy = "tf_deploy")
load("//tf/private:library.bzl", _tf_library = "tf_library")

tf_library = _tf_library
tf_deploy = _tf_deploy
