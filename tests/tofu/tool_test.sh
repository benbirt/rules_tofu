#!/usr/bin/env bash
# Tests //tofu used as a tool in another target's runfiles, where the launcher
# has no runfiles tree of its own. TOFU is the launcher's runfiles path.
#
# Steps:
#   1. The launcher finds the binary with only RUNFILES_DIR set and with only
#      TEST_SRCDIR set. When RUNFILES_DIR points to a directory without the
#      binary, it falls through to TEST_SRCDIR. With neither set, it fails and
#      reports that it cannot find the binary.
#   2. With BUILD_WORKING_DIRECTORY set (as under `bazel run`) and started from
#      a different directory, the launcher runs `tofu` in
#      BUILD_WORKING_DIRECTORY. File names relative to that directory,
#      including one with a space, are passed through `xargs -0` to
#      `fmt -check -diff`, which must fail and name both files. This mirrors
#      rules_lint's format.sh, which changes to that directory, leaves
#      BUILD_WORKING_DIRECTORY set and passes relative names through `xargs`.
#   3. Without BUILD_WORKING_DIRECTORY (as under `bazel test`, for example
#      rules_lint's format_test), the launcher runs `tofu` in the caller's
#      current directory.

set -euo pipefail

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

RUNFILES="${TEST_SRCDIR:?run this as a Bazel test}"
launcher="${RUNFILES}/${TOFU:?set by the test target}"
[[ -x "${launcher}" ]] || fail "no launcher at ${launcher}"

# Remove RUNFILES_DIR, TEST_SRCDIR, BUILD_WORKING_DIRECTORY and
# BUILD_WORKSPACE_DIRECTORY so each step states what the launcher receives.
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
  # shellcheck disable=SC2086 # split vars into NAME=VALUE arguments
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
# Start in another directory so the launcher must change to
# BUILD_WORKING_DIRECTORY itself.
cd "${TEST_TMPDIR}"
lint() {
  files | clean env "RUNFILES_DIR=${RUNFILES}" "BUILD_WORKING_DIRECTORY=${work}" xargs -0 "${launcher}" "$@"
}
if out="$(lint fmt -check -diff 2>&1)"; then
  fail "fmt -check passed on unformatted files: ${out}"
fi
for f in main.tf "sub/spaced name.tofu"; do
  grep -q -x -F "${f}" <<<"${out}" || fail "fmt -check didn't name ${f}: ${out}"
done
grep -q -x -F '+variable "name" {}' <<<"${out}" || fail "fmt -check -diff didn't diff the files: ${out}"

echo "--- 3. in the caller's directory ---" >&2
echo "${unformatted}" >"${work}/main.tf"
cd "${work}"
if out="$(clean env "TEST_SRCDIR=${RUNFILES}" "${launcher}" fmt -check main.tf 2>&1)"; then
  fail "fmt -check passed on an unformatted file in the caller's directory: ${out}"
fi
grep -q -x -F 'main.tf' <<<"${out}" || fail "fmt -check didn't name main.tf: ${out}"

echo "OK: //tofu runs as another target's tool" >&2
