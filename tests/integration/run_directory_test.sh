#!/usr/bin/env bash
# Driver for //tests/integration:run_directory_test. The
# bazel_integration_test rule sets BIT_BAZEL_BINARY and BIT_WORKSPACE_DIR (the
# run_directory sub-workspace).
#
# Steps:
#   1. `bazel run //app:deploy.apply` creates a resource and reads greeting.txt
#      through path.module. The deploy's backend "local" has a relative path,
#      which tofu resolves inside deploy.rules_tofu-state under bazel-bin,
#      leaving no .tfstate, .terraform or .terraform.lock.hcl elsewhere under
#      bazel-bin/app.
#   2. Building //app:deploy.destroy and running the .destroy launcher
#      directly by relative path from bazel-bin (without the variables
#      `bazel run` sets) finds that state and destroys the resource: the
#      launchers are ordinary executables sharing one run directory.
#   3. `bazel run //app:deploy.plan` plans against the same, now empty, state.
#   4. bazel cquery --output=files reports only stamp files as //app:deploy's
#      default outputs, excluding the work tree and provider packages (issue
#      #156), and lists each launcher's script plus deploy.validate.stamp as
#      the launcher's default outputs (issue #143).

set -euo pipefail

_TMPDIR="${TEST_TMPDIR:-/tmp}"

# The nested Bazel's caches and the `bazel` function that runs it.
# shellcheck source=tests/integration/nested_bazel.sh
source "${BIT_WORKSPACE_DIR}/../nested_bazel.sh"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

# Nothing here writes to the workspace, so it is used where it is.
cd "${BIT_WORKSPACE_DIR}"

echo "--- 1. bazel run the apply ---" >&2
out="$(bazel run //app:deploy.apply -- -auto-approve 2>&1)" || fail "the apply failed: ${out}"
echo "${out}" >&2
grep -q -F 'Resources: 1 added' <<<"${out}" || fail "the apply didn't create the resource"
grep -q -F 'hello from the run directory' <<<"${out}" || fail "the apply didn't read greeting.txt through path.module"

BIN="$(bazel info bazel-bin)"
STATE="${BIN}/app/deploy.rules_tofu-state/work/app/state/dev.tfstate"
grep -q -F '"terraform_data"' "${STATE}" || fail "no state for the resource at ${STATE}"
stray="$(find "${BIN}/app" -path "${BIN}/app/deploy.rules_tofu-state" -prune -o \( -name '*.tfstate' -o -name .terraform -o -name .terraform.lock.hcl \) -print)"
[[ -z "${stray}" ]] || fail "tofu wrote outside the run directory: ${stray}"

echo "--- 2. run the built destroy launcher directly ---" >&2
bazel build //app:deploy.destroy >&2 || fail "building the destroy launcher failed"
out="$(cd "${BIN}" && env -u BUILD_WORKSPACE_DIRECTORY -u BUILD_WORKING_DIRECTORY app/deploy.destroy.sh -auto-approve 2>&1)" ||
  fail "the destroy launcher failed when run directly: ${out}"
echo "${out}" >&2
grep -q -F 'Resources: 1 destroyed' <<<"${out}" || fail "the destroy launcher didn't find the state the apply wrote"

echo "--- 3. plan against the same state ---" >&2
out="$(bazel run //app:deploy.plan 2>&1)" || fail "the plan failed: ${out}"
echo "${out}" >&2
grep -q -F '1 to add' <<<"${out}" || fail "the plan didn't see the destroyed resource as one to add"

echo "--- 4. default outputs ---" >&2
files="$(bazel cquery --output=files //app:deploy 2>/dev/null)" || fail "cquery of //app:deploy failed"
echo "${files}" >&2
grep -q -F '/app/deploy.validate.stamp' <<<"${files}" || fail "//app:deploy default outputs lack deploy.validate.stamp"
if grep -q -v '\.stamp$' <<<"${files}"; then
  fail "//app:deploy default outputs include non-stamp files: ${files}"
fi
for command in plan apply destroy; do
  files="$(bazel cquery --output=files "//app:deploy.${command}" 2>/dev/null)" || fail "cquery of //app:deploy.${command} failed"
  echo "${files}" >&2
  grep -q -F "/app/deploy.${command}.sh" <<<"${files}" || fail "//app:deploy.${command} default outputs lack deploy.${command}.sh"
  grep -q -F '/app/deploy.validate.stamp' <<<"${files}" || fail "//app:deploy.${command} default outputs lack deploy.validate.stamp"
done

# The nested server would otherwise live until the test's process group is
# killed.
bazel shutdown >/dev/null 2>&1 || true

echo "OK: apply, destroy and plan shared the deploy's run directory" >&2
