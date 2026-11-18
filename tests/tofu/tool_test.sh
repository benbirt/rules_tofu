#!/usr/bin/env bash
# Test of //tofu as a tool in another target's runfiles, where it has no
# runfiles tree of its own. TOFU is the launcher's path in the runfiles.
#
# Steps:
#   1. The launcher finds the tofu binary through either of RUNFILES_DIR and
#      TEST_SRCDIR on its own, skips a variable that points somewhere without
#      the binary, and says where it looked when neither leads to it.
#   2. With BUILD_WORKING_DIRECTORY set, as under `bazel run`, it runs tofu in
#      that directory: rules_lint's format.sh changes to it, leaves the
#      variable set, and passes file names relative to it through `xargs`,
#      first to `fmt -check -diff` and then to `fmt`.
#   3. Without BUILD_WORKING_DIRECTORY, as under `bazel test` (rules_lint's
#      format_test), it runs tofu in the caller's directory.

set -euo pipefail

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

RUNFILES="${TEST_SRCDIR:?run this as a Bazel test}"
launcher="${RUNFILES}/${TOFU:?set by the test target}"
[[ -x "${launcher}" ]] || fail "no launcher at ${launcher}"

# Starts from nothing Bazel set, so each step names what the launcher gets.
clean() {
  env -u RUNFILES_DIR -u TEST_SRCDIR \
    -u BUILD_WORKING_DIRECTORY -u BUILD_WORKSPACE_DIRECTORY "$@"
}

echo "--- 1. finding the binary ---" >&2
empty="${TEST_TMPDIR}/empty"
mkdir -p "${empty}"
for vars in \
  "RUNFILES_DIR=${RUNFILES}" \
  "TEST_SRCDIR=${RUNFILES}" \
  "RUNFILES_DIR=${empty} TEST_SRCDIR=${RUNFILES}"; do
  # shellcheck disable=SC2086 # one word per variable
  out="$(clean env ${vars} "${launcher}" version 2>&1)" || fail "with ${vars}: ${out}"
  grep -q '^OpenTofu v' <<<"${out}" || fail "with ${vars}, not tofu's version: ${out}"
done
if out="$(clean "${launcher}" version 2>&1)"; then
  fail "ran with no runfiles to find: ${out}"
fi
grep -q -F 'cannot find the tofu binary' <<<"${out}" || fail "an unhelpful error with no runfiles: ${out}"

echo "--- 2. in BUILD_WORKING_DIRECTORY ---" >&2
work="${TEST_TMPDIR}/work dir"
mkdir -p "${work}/sub"
unformatted='variable   "name" {}'
echo "${unformatted}" >"${work}/main.tf"
echo "${unformatted}" >"${work}/sub/spaced name.tofu"
files() {
  printf '%s\0' main.tf "sub/spaced name.tofu"
}
# The launcher is started somewhere else and has to change directory itself.
cd "${TEST_TMPDIR}"
lint() {
  files | clean env "RUNFILES_DIR=${RUNFILES}" "BUILD_WORKING_DIRECTORY=${work}" xargs -0 "${launcher}" "$@"
}
if out="$(lint fmt -check -diff 2>&1)"; then
  fail "fmt -check passed on unformatted files: ${out}"
fi
for f in main.tf "sub/spaced name.tofu"; do
  grep -q -F "${f}" <<<"${out}" || fail "fmt -check didn't name ${f}: ${out}"
done
lint fmt >&2 || fail "fmt failed"
lint fmt -check -diff >&2 || fail "fmt -check still fails after fmt"
grep -q -x -F 'variable "name" {}' "${work}/sub/spaced name.tofu" || fail "fmt didn't rewrite the file: $(cat "${work}/sub/spaced name.tofu")"

echo "--- 3. in the caller's directory ---" >&2
echo "${unformatted}" >"${work}/main.tf"
cd "${work}"
if out="$(clean env "TEST_SRCDIR=${RUNFILES}" "${launcher}" fmt -check main.tf 2>&1)"; then
  fail "fmt -check passed on an unformatted file in the caller's directory: ${out}"
fi
grep -q -x -F 'main.tf' <<<"${out}" || fail "fmt -check didn't name main.tf: ${out}"

echo "OK: //tofu runs as another target's tool" >&2
