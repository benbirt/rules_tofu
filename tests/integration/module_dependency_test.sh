#!/usr/bin/env bash
# Driver for //tests/integration:module_dependency_*_test. The
# bazel_integration_test rule sets BIT_BAZEL_BINARY and BIT_WORKSPACE_DIR (the
# module_dependency workspace), and the test sets TOFU_VERSION. Unlike
# assert_bazel_fails.sh, every step here must succeed:
#
#   1. Building //... validates //app, which calls a tf_library from another
#      Bazel module by its module_source, which calls one from a third.
#   2. Each of those libraries also builds on its own, as a root module in
#      its own repository.
#   3. `bazel run //app:app.plan` plans against the same manifest, offline.
#   4. The toolchain's tofu reports TOFU_VERSION, so a version that didn't
#      take is caught.
#   5. `bazel test //app:app_test` runs the deploy's tf_test, whose run block
#      loads a module from the manifest. It passes on every pinned tofu, and
#      leaves the JSON log in its outputs exactly when that tofu's `test`
#      has -json-into, which 1.8 doesn't (issue #125).
#
# The module manifest is tofu's internal format, so there is one test per tofu
# version rules_tofu pins.

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
# Only the exec platform's tofu repository has been fetched.
tofus=("$(bazel info output_base)"/external/rules_tofu++tofu+tofu_*/tofu)
[[ ${#tofus[@]} -eq 1 && -x "${tofus[0]}" ]] || fail "expected one fetched tofu binary, got: ${tofus[*]}"
tofu="${tofus[0]}"
# Captured first: a failed help must not read as "no such flag".
help="$("${tofu}" test -help)" || fail "'${tofu} test -help' failed"
bazel test //app:app_test --test_output=all >&2 || fail "the tf_test failed"
outputs_dir="$(bazel info bazel-testlogs)/app/app_test/test.outputs"
# Bazel zips a test's undeclared outputs unless told not to, and makes no
# zip when there are none.
json_log=""
if [[ -f "${outputs_dir}/tofu-test.json" ]]; then
  json_log="$(cat "${outputs_dir}/tofu-test.json")"
elif [[ -f "${outputs_dir}/outputs.zip" ]]; then
  json_log="$(unzip -p "${outputs_dir}/outputs.zip" tofu-test.json 2>/dev/null || true)"
fi
if grep -q -F -- '-json-into' <<<"${help}"; then
  grep -q -F '"@message"' <<<"${json_log}" || fail "tofu ${TOFU_VERSION} has -json-into, but the test left no JSON log in ${outputs_dir}"
else
  # Not even an empty file: the runner must not have passed the flag.
  [[ ! -e "${outputs_dir}/tofu-test.json" ]] || fail "tofu ${TOFU_VERSION} has no -json-into, yet the test left a tofu-test.json"
  zipped=""
  if [[ -f "${outputs_dir}/outputs.zip" ]]; then
    zipped="$(unzip -Z1 "${outputs_dir}/outputs.zip" 2>/dev/null || true)"
  fi
  if grep -q -x -F 'tofu-test.json' <<<"${zipped}"; then
    fail "tofu ${TOFU_VERSION} has no -json-into, yet the test's outputs.zip holds a tofu-test.json"
  fi
fi

echo "OK: with tofu ${TOFU_VERSION}, modules from other Bazel modules were called by module_source" >&2
