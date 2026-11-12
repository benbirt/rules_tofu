#!/usr/bin/env bash
# Driver for //tests/integration:fmt_*_test. The bazel_integration_test rule
# sets BIT_BAZEL_BINARY and BIT_WORKSPACE_DIR (the fmt workspace), and the
# test sets TOFU_VERSION. Every step must succeed:
#
#   1. A target whose `srcs` are all in a subdirectory passes `.fmt_check`.
#   2. A target with unformatted `srcs` in subdirectories fails `.fmt_check`,
#      naming each of them: a library's .tf and .tofu files, and a tf_test's
#      .tftest.hcl.
#   3. `.fmt` reformats exactly those files: afterwards `.fmt_check` passes
#      and the package's file outside `srcs` is byte-identical.
#   4. Targets without `srcs` have no `.fmt` or `.fmt_check`.
#   5. The toolchain's tofu reports TOFU_VERSION, so a version that didn't
#      take is caught: which extensions `tofu fmt` accepts by name is the
#      binary's to decide.
#   6. Everything in the workspace builds, so the fixture stays a shape the
#      rules accept (it once listed the test file in a library's `srcs`,
#      which nothing noticed: the steps above only format and query).

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

# The workspace sits among read-only runfiles and `.fmt` writes to it, so
# work on a copy whose root selects TOFU_VERSION, pointing its
# local_path_override at the rules_tofu sources beside the original.
RULES_TOFU="$(cd "${BIT_WORKSPACE_DIR}/../../.." && pwd)"
WS="${_TMPDIR}/fmt"
rm -rf "${WS}"
cp -RL "${BIT_WORKSPACE_DIR}" "${WS}"
chmod -R u+w "${WS}"
sed -i.bak "s|path = \"../../..\"|path = \"${RULES_TOFU}\"|" "${WS}/MODULE.bazel"
rm "${WS}/MODULE.bazel.bak"
echo "tofu.version(version = \"${TOFU_VERSION}\")" >>"${WS}/MODULE.bazel"
cd "${WS}"

echo "--- 1. srcs only in a subdirectory ---" >&2
bazel test //sub_only:deploy.fmt_check --test_output=errors >&2 || fail "fmt_check of a target whose srcs are all in a subdirectory failed"

echo "--- 2. unformatted srcs in subdirectories fail the check ---" >&2
cp nested/not_src.tf "${_TMPDIR}/not_src.tf.before"
if out="$(bazel test //nested:lib.fmt_check --test_output=all 2>&1)"; then
  echo "${out}" >&2
  fail "fmt_check passed with unformatted files in subdirectories"
fi
echo "${out}" >&2
for f in sub/bad.tf sub/types.tofu; do
  grep -q -F "nested/${f}" <<<"${out}" || fail "fmt_check didn't name ${f}"
done
grep -q -F "not_src.tf" <<<"${out}" && fail "fmt_check looked at a file outside srcs"
grep -q -F "plan.tftest.hcl" <<<"${out}" && fail "the library's fmt_check looked at the tf_test's file"
if out="$(bazel test //nested:tests.fmt_check --test_output=all 2>&1)"; then
  echo "${out}" >&2
  fail "fmt_check passed with an unformatted test file"
fi
echo "${out}" >&2
grep -q -F "nested/tests/plan.tftest.hcl" <<<"${out}" || fail "the tf_test's fmt_check didn't name tests/plan.tftest.hcl"
grep -q -F "bad.tf" <<<"${out}" && fail "the tf_test's fmt_check looked at the library's files"

echo "--- 3. fmt rewrites the srcs, and only them ---" >&2
bazel run //nested:lib.fmt >&2 || fail "fmt failed"
bazel run //nested:tests.fmt >&2 || fail "fmt of the tf_test failed"
bazel test //nested:lib.fmt_check //nested:tests.fmt_check --test_output=errors >&2 || fail "fmt_check still fails after fmt"
grep -q -F 'run "plan"' nested/tests/plan.tftest.hcl || fail "fmt didn't rewrite nested/tests/plan.tftest.hcl: $(cat nested/tests/plan.tftest.hcl)"
cmp nested/not_src.tf "${_TMPDIR}/not_src.tf.before" || fail "fmt rewrote a file outside srcs"
grep -q -F 'variable "cidr"' nested/sub/bad.tf || fail "fmt didn't rewrite nested/sub/bad.tf: $(cat nested/sub/bad.tf)"

echo "--- 4. no srcs, no fmt targets ---" >&2
targets="$(bazel query //nested:all)"
echo "${targets}" >&2
grep -q -x -F '//nested:no_srcs_lib' <<<"${targets}" || fail "the library without srcs is missing"
grep -q -x -F '//nested:no_srcs_deploy.plan' <<<"${targets}" || fail "the deploy without srcs is missing"
grep -q '^//nested:no_srcs_.*\.fmt' <<<"${targets}" && fail "a target without srcs has fmt targets"

echo "--- 5. check the tofu version ---" >&2
# Only the exec platform's tofu repository has been fetched.
versions="$(for t in "$(bazel info output_base)"/external/rules_tofu++tofu+tofu_*/tofu; do "$t" version; done)"
grep -q -F "v${TOFU_VERSION}" <<<"${versions}" || fail "expected tofu ${TOFU_VERSION}, got: ${versions}"

echo "--- 6. the whole workspace builds ---" >&2
bazel build //... >&2 || fail "the fmt workspace doesn't build"

echo "OK: with tofu ${TOFU_VERSION}, fmt and fmt_check act on srcs by name" >&2
