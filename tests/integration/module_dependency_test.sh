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
#   4. The toolchain's tofu reports TOFU_VERSION, catching a version override
#      that did not take effect.
#   5. `bazel test //app:app_test` runs the deploy's tf_test, whose run block
#      loads a module from the manifest, and checks that tofu-test.json is
#      written to its test outputs. The runner passes `tofu test` a flag
#      (-json-into) that OpenTofu before 1.12 lacks, and tf_test once ran on no
#      version except the default (issue #125).
#   6. `bazel run //app:app.apply`, a deploy with no backend block and the
#      opt-in, leaves tofu's default terraform.tfstate in the run directory and
#      no backend record.
#   7. `bazel run //app:durable.apply`, a deploy with a `backend "local"` and no
#      opt-in, is refused while the variable in its paths makes them relative,
#      and runs once the variable is an absolute path outside Bazel's output
#      tree, writing state there. The paths come from an override file.
#
# Runs once per tofu version rules_tofu pins because the module manifest and
# the backend record `tofu init` writes, which the runner reads to decide
# whether state is ephemeral, are tofu's internal formats.

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

echo "--- 4. check the tofu version ---" >&2
# Only the exec platform's tofu repository has been fetched.
versions="$(for t in "$(bazel info output_base)"/external/rules_tofu++tofu+tofu_*/tofu; do "$t" version; done)"
grep -q -F "v${TOFU_VERSION}" <<<"${versions}" || fail "expected tofu ${TOFU_VERSION}, got: ${versions}"

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

echo "--- 6. apply with no backend block ---" >&2
out="$(bazel run //app:app.apply -- -auto-approve 2>&1)" || fail "the apply with no backend block failed: ${out}"
echo "${out}" >&2
RUN_DIR="$(bazel info bazel-bin)/app/app.rules_tofu-state/work/app"
grep -q -F 'www.app.example.com' "${RUN_DIR}/terraform.tfstate" || fail "no state at ${RUN_DIR}/terraform.tfstate"
[[ ! -e "${RUN_DIR}/.terraform/terraform.tfstate" ]] || fail "tofu init recorded a backend for a deploy with no backend block: $(cat "${RUN_DIR}/.terraform/terraform.tfstate")"

echo "--- 7. the ephemeral-state gate reads the backend tofu init recorded ---" >&2
if out="$(bazel run //app:durable.apply -- -auto-approve 2>&1)"; then
  fail "the apply ran against a backend \"local\" with relative paths: ${out}"
fi
echo "${out}" >&2
grep -q -F 'its `path` ("state/app.tfstate") is relative' <<<"${out}" || fail "the refusal doesn't name the path tofu resolved"

STATE_ROOT="${_TMPDIR}/durable_state"
sed -i.bak "s|\"state_root\": \"state\"|\"state_root\": \"${STATE_ROOT}\"|" app/BUILD.bazel
rm app/BUILD.bazel.bak
out="$(bazel run //app:durable.apply -- -auto-approve 2>&1)" || fail "the apply against a backend \"local\" outside Bazel's output tree failed: ${out}"
echo "${out}" >&2
grep -q -F 'www.app.example.com' "${STATE_ROOT}/app.tfstate" || fail "no state at ${STATE_ROOT}/app.tfstate"

echo "OK: with tofu ${TOFU_VERSION}, modules from other Bazel modules were called by module_source" >&2
