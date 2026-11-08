#!/usr/bin/env bash
# Driver for the negative //tests/integration:*_test targets. The
# bazel_integration_test rule sets BIT_BAZEL_BINARY and BIT_WORKSPACE_DIR; the
# test's env sets:
#   CASES        — one case per line, `name|command|target|fragment`: the
#                  nested Bazel command (build or run), the target inside the
#                  sub-workspace, and a substring expected in its stderr
#   ONLY_CASES   — optional, comma-separated names: run only these cases
#                  (e.g. `--test_env=ONLY_CASES=stale_lock` from the outer Bazel)
# Every case must fail with its fragment in the output; anything else fails
# the test. A workspace's cases all run against one nested Bazel, so after the
# first they are incremental builds; every case runs even after a failure, and
# the summary at the end names the ones that failed.

set -uo pipefail

# Use TEST_TMPDIR for our temp files; this is the per-test scratch area
# provided by the outer Bazel.
_TMPDIR="${TEST_TMPDIR:-/tmp}"

# Bazel sees TEST_TMPDIR and puts the nested Bazel's output user root under
# it, so every test would extract an install base and fetch each repository
# from nothing (the Go SDK alone is ten seconds). Give it the outer Bazel's
# user root instead, Bazel's own default for the platform (HOME is inherited
# by bazel_integration_test): the install base and the repository and repo
# contents caches are then shared with the outer Bazel and across tests,
# while --output_base below stays per test. The nested `bazel` is a bazelisk
# wrapper that likewise caches its Bazel download under $PWD unless told
# otherwise; use its per-user cache, which setup-bazel saves on CI.
_user="${USER:-$(id -un)}"
if [[ "$(uname -s)" == Darwin ]]; then
  NESTED_USER_ROOT="/var/tmp/_bazel_${_user}"
  : "${BAZELISK_HOME:=${HOME}/Library/Caches/bazelisk}"
else
  NESTED_USER_ROOT="${XDG_CACHE_HOME:-${HOME}/.cache}/bazel/_bazel_${_user}"
  : "${BAZELISK_HOME:=${XDG_CACHE_HOME:-${HOME}/.cache}/bazelisk}"
fi
export BAZELISK_HOME

# Force the nested Bazel to use its own output base inside the test's temp
# directory.  Without this, it inherits the global --output_base from
# ~/.bazelrc (set by setup-bazel), which points to the SAME output base the
# outer Bazel is using.  The nested Bazel then tries to kill the outer
# Bazel's server to take over — deadlocking because the outer Bazel is the
# process running this test.
#
# See: https://github.com/bazel-contrib/setup-bazel/issues/108
NESTED_OUTPUT_BASE="${_TMPDIR}/nested_output_base"

# --output_user_root and --output_base are STARTUP options (before the
# command), so they override anything set in ~/.bazelrc.
bazel() {
  "${BIT_BAZEL_BINARY}" --output_user_root="${NESTED_USER_ROOT}" --output_base="${NESTED_OUTPUT_BASE}" "$@"
}

# The log of the case in progress, dumped if the test is killed by timeout.
LOG=""
dump_log() {
  if [[ -n "${LOG}" && -s "${LOG}" ]]; then
    echo "--- nested bazel log of the case in progress (${LOG}) ---" >&2
    cat "${LOG}" >&2
    echo "--- end nested bazel log ---" >&2
  fi
}
trap dump_log EXIT

# Diagnostics — these go to stderr so they appear in test output even on timeout.
echo "=== assert_bazel_fails ===" >&2
echo "  BIT_BAZEL_BINARY:   ${BIT_BAZEL_BINARY}" >&2
echo "  BIT_WORKSPACE_DIR:  ${BIT_WORKSPACE_DIR}" >&2
echo "  TEST_TMPDIR:        ${_TMPDIR}" >&2
echo "  NESTED_OUTPUT_BASE: ${NESTED_OUTPUT_BASE}" >&2
echo "  ONLY_CASES:         ${ONLY_CASES:-}" >&2
echo "  date:               $(date -u)" >&2

cd "${BIT_WORKSPACE_DIR}"

only=",${ONLY_CASES:-},"
passed=()
failed=()
ran=0
while IFS='|' read -r name command target pattern; do
  [[ -n "${name}" ]] || continue
  if [[ "${only}" != ",," && "${only}" != *",${name},"* ]]; then
    continue
  fi
  ran=$((ran + 1))
  LOG="${_TMPDIR}/${name}.log"
  echo "--- ${name}: bazel ${command} ${target}, expecting '${pattern}' ---" >&2
  start=${SECONDS}
  bazel "${command}" "${target}" >"${LOG}" 2>&1
  rc=$?
  elapsed=$((SECONDS - start))
  if [[ ${rc} -eq 0 ]]; then
    verdict="FAIL: ${name}: ${command} of ${target} succeeded; expected failure"
  elif ! grep -q -F -- "${pattern}" "${LOG}"; then
    verdict="FAIL: ${name}: ${target} failed (exit ${rc}) but stderr did not contain '${pattern}'"
  else
    verdict=""
  fi
  if [[ -n "${verdict}" ]]; then
    echo "${verdict}" >&2
    dump_log
    failed+=("${name}")
  else
    echo "OK: ${name} failed with '${pattern}' (exit ${rc}, ${elapsed}s)" >&2
    passed+=("${name}")
  fi
  LOG=""
done <<<"${CASES}"

# The nested server would otherwise live until the test's process group is
# killed.
bazel shutdown >/dev/null 2>&1 || true

echo "=== ${ran} cases: ${#passed[@]} passed, ${#failed[@]} failed ===" >&2
if [[ ${ran} -eq 0 ]]; then
  echo "FAIL: no case matched ONLY_CASES='${ONLY_CASES:-}'" >&2
  exit 1
fi
if [[ ${#failed[@]} -gt 0 ]]; then
  echo "FAIL: ${failed[*]}" >&2
  exit 1
fi
