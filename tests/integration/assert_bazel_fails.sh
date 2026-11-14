#!/usr/bin/env bash
# Driver for the negative //tests/integration:*_test targets (and
# custom_toolchain_test). The bazel_integration_test rule sets BIT_BAZEL_BINARY
# and BIT_WORKSPACE_DIR; the test's env sets:
#   CASES      - one case per line, `name|command|target|fragment`: the nested
#                Bazel command (build or run), the target inside the
#                sub-workspace, and a substring expected in its stderr
#   ONLY_CASES - optional, comma-separated names: run only these cases
#                (e.g. `--test_env=ONLY_CASES=stale_lock` from the outer Bazel)
# Every case must fail with its fragment in the output; anything else fails
# the test. A sub-workspace's cases all run against one nested Bazel, so after
# the first they are incremental builds; every case runs even after a failure,
# and the summary at the end names the ones that failed.

set -uo pipefail

# Use TEST_TMPDIR for our temp files; this is the per-test scratch area
# provided by the outer Bazel.
_TMPDIR="${TEST_TMPDIR:-/tmp}"

# The nested Bazel's caches and the `bazel` function that runs it.
# shellcheck source=tests/integration/nested_bazel.sh
source "${BIT_WORKSPACE_DIR}/../nested_bazel.sh"

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

# Diagnostics go to stderr so they appear in test output even on timeout.
echo "=== assert_bazel_fails ===" >&2
echo "  BIT_BAZEL_BINARY:   ${BIT_BAZEL_BINARY}" >&2
echo "  BIT_WORKSPACE_DIR:  ${BIT_WORKSPACE_DIR}" >&2
echo "  TEST_TMPDIR:        ${_TMPDIR}" >&2
echo "  NESTED_OUTPUT_BASE: ${NESTED_OUTPUT_BASE}" >&2
echo "  ONLY_CASES:         ${ONLY_CASES:-}" >&2
echo "  date:               $(date -u)" >&2

cd "${BIT_WORKSPACE_DIR}"

only=",${ONLY_CASES:-},"
# Every non-empty line of CASES is a case the loop below must run.
expected="$(grep -c . <<<"${CASES}" || true)"
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
  # Not the loop's stdin, which is CASES: a `run` case whose program reads
  # its input (`tofu` asking for a variable, or for approval) would consume
  # the remaining cases, and the loop would end early with nothing failed.
  bazel "${command}" "${target}" >"${LOG}" 2>&1 </dev/null
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
if [[ -z "${ONLY_CASES:-}" && ${ran} -ne ${expected} ]]; then
  echo "FAIL: ran ${ran} of ${expected} cases" >&2
  exit 1
fi
if [[ ${#failed[@]} -gt 0 ]]; then
  echo "FAIL: ${failed[*]}" >&2
  exit 1
fi
