# Deliberately crosses a Bazel repository boundary by relative path; do not change source.
module "other" { source = "../../external/rules_tofu+/tf" }
