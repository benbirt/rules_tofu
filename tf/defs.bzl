"""Rules for OpenTofu and Terraform-compatible configuration.

`tf_library` bundles reusable configuration files, `tf_deploy` binds variable
values and generates `.plan`, `.apply` and `.destroy` targets, and `tf_test`
runs `tofu test` as a Bazel test:

    load("@rules_tofu//tf:defs.bzl", "tf_deploy", "tf_library", "tf_test")

See the [README](https://github.com/benbirt/rules_tofu#rules) for a full guide.
"""

load("//tf/private:deploy.bzl", _tf_deploy = "tf_deploy")
load("//tf/private:library.bzl", _tf_library = "tf_library")
load("//tf/private:test.bzl", _tf_test = "tf_test")

tf_library = _tf_library
tf_deploy = _tf_deploy
tf_test = _tf_test
