#!/usr/bin/env bash
# Driver for //tests/integration:module_dependency_*_test. The
# bazel_integration_test rule sets BIT_BAZEL_BINARY and BIT_WORKSPACE_DIR (the
# module_dependency sub-workspace), and the test sets TOFU_VERSION.
#
# Steps:
#   1. `bazel build //...` validates //app (which calls @network_module//vpc by
#      module_source, which in turn calls @dns_module//:dns) and //app:app_test.
#   2. @network_module//vpc and @dns_module//:dns also build on their own, as
#      root modules in their own repositories.
#   3. `bazel run //app:app.plan` plans offline against the generated module
#      manifest and checks the module chain's output.
#   4. `bazel run @rules_tofu//tofu`, started in app/, runs the tofu of
#      TOFU_VERSION there with the arguments it is given. That is the pinned
#      binary as a consumer of rules_tofu reaches it, and it catches a version
#      override that did not take effect.
#   5. `bazel test //app:app_test` runs the deploy's tf_test, whose run block
#      loads a module from the manifest, and checks that tofu-test.json is
#      written to its test outputs. The runner passes `tofu test` a flag
#      (-json-into) that OpenTofu before 1.12 lacks, and tf_test once ran on no
#      version except the default (issue #125).
#
# Runs once per tofu version rules_tofu pins because the module manifest is
# tofu's internal format.

set -euo pipefail

_TMPDIR="${TEST_TMPDIR:-/tmp}"

# The nested Bazel's caches and the `bazel` function that runs it.
# shellcheck source=tests/integration/nested_bazel.sh
source "${BIT_WORKSPACE_DIR}/../nested_bazel.sh"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

[[ -n "${TOFU_VERSION:-}" ]] || fail "TOFU_VERSION is not set"

# The workspace sits among read-only runfiles, so work on a copy whose root
# selects TOFU_VERSION, pointing its local_path_override at the rules_tofu
# sources beside the original.
RULES_TOFU="$(cd "${BIT_WORKSPACE_DIR}/../../.." && pwd)"
WS="${_TMPDIR}/module_dependency"
rm -rf "${WS}"
cp -RL "${BIT_WORKSPACE_DIR}" "${WS}"
chmod -R u+w "${WS}"
sed -i.bak "s|path = \"../../..\"|path = \"${RULES_TOFU}\"|" "${WS}/MODULE.bazel"
rm "${WS}/MODULE.bazel.bak"
echo "tofu.version(version = \"${TOFU_VERSION}\")" >>"${WS}/MODULE.bazel"
cd "${WS}"

echo "--- 1. build //... ---" >&2
bazel build //... >&2 || fail "building the workspace failed"

echo "--- 2. build the libraries in their own repositories ---" >&2
bazel build @network_module//vpc @dns_module//:dns >&2 || fail "building the external libraries failed"

echo "--- 3. plan ---" >&2
out="$(bazel run //app:app.plan 2>&1)" || fail "the plan failed: ${out}"
echo "${out}" >&2
grep -q -F 'www.app.example.com' <<<"${out}" || fail "the plan doesn't show the module chain's output"

echo "--- 4. run the pinned tofu ---" >&2
version="$(cd app && bazel run @rules_tofu//tofu -- version)" || fail "bazel run @rules_tofu//tofu failed"
grep -q -x -F "OpenTofu v${TOFU_VERSION}" <<<"${version}" || fail "expected tofu ${TOFU_VERSION}, got: ${version}"
# A relative path reaches a file only if tofu runs where Bazel was started.
printf 'variable   "unformatted" {}\n' >app/scratch.tf
out="$(cd app && bazel run @rules_tofu//tofu -- fmt -list=false -write=false -diff scratch.tf)" || fail "tofu fmt of a file in the directory Bazel was started in failed"
grep -q -x -F '+variable "unformatted" {}' <<<"${out}" || fail "tofu fmt did not print the diff of app/scratch.tf: ${out}"
rm app/scratch.tf

echo "--- 5. run the tf_test ---" >&2
bazel test //app:app_test --test_output=all >&2 || fail "the tf_test failed"
outputs_dir="$(bazel info bazel-testlogs)/app/app_test/test.outputs"
# Bazel zips a test's undeclared outputs unless told not to.
json_log=""
if [[ -f "${outputs_dir}/tofu-test.json" ]]; then
  json_log="$(cat "${outputs_dir}/tofu-test.json")"
elif [[ -f "${outputs_dir}/outputs.zip" ]]; then
  json_log="$(unzip -p "${outputs_dir}/outputs.zip" tofu-test.json 2>/dev/null || true)"
fi
grep -q -F '"@message"' <<<"${json_log}" || fail "the test left no JSON log (tofu-test.json) in ${outputs_dir}"

echo "OK: with tofu ${TOFU_VERSION}, modules from other Bazel modules were called by module_source" >&2
