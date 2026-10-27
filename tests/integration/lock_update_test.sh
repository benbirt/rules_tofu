#!/usr/bin/env bash
# Driver for //tests/integration:lock_update_test. The bazel_integration_test
# rule sets BIT_BAZEL_BINARY and BIT_WORKSPACE_DIR (the lock_update
# workspace). Unlike assert_bazel_fails.sh, every step here must succeed:
#
#   1. `bazel run @tf_providers//:pin` writes the first lock, although the
#      workspace's tf_library already names `@tf_providers//hashicorp/null`,
#      which has no package in the hub until it is locked.
#   2. A build records MODULE.bazel.lock.
#   3. hashicorp/null is bumped the way Dependabot or Renovate does it: in
#      versions.tf and .terraform.lock.hcl, and nowhere else.
#   4. The build passes with --lockfile_mode=error and leaves MODULE.bazel.lock
#      byte-identical, so the bot's change is complete on its own.

set -euo pipefail

_TMPDIR="${TEST_TMPDIR:-/tmp}"

# The workspace sits among read-only runfiles, so work on a copy, pointing its
# local_path_override at the rules_tofu sources beside the original.
RULES_TOFU="$(cd "${BIT_WORKSPACE_DIR}/../../.." && pwd)"
WS="${_TMPDIR}/lock_update"
rm -rf "${WS}"
cp -RL "${BIT_WORKSPACE_DIR}" "${WS}"
chmod -R u+w "${WS}"
sed -i.bak "s|path = \"../../..\"|path = \"${RULES_TOFU}\"|" "${WS}/MODULE.bazel"
rm "${WS}/MODULE.bazel.bak"
cd "${WS}"

# Its own output base, as in assert_bazel_fails.sh: the one in ~/.bazelrc is
# the outer Bazel's. pin runs `bazel query` itself, so it gets the same
# wrapper through $BAZEL.
NESTED_OUTPUT_BASE="${_TMPDIR}/nested_output_base"
BAZEL="${_TMPDIR}/bazel"
cat >"${BAZEL}" <<EOF
#!/usr/bin/env bash
exec "${BIT_BAZEL_BINARY}" --output_base="${NESTED_OUTPUT_BASE}" "\$@"
EOF
chmod +x "${BAZEL}"
export BAZEL

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

echo "--- 1. first pin, with the provider already named ---" >&2
[[ ! -e .terraform.lock.hcl ]] || fail "the workspace already has a lock"
"${BAZEL}" run @tf_providers//:pin >&2 || fail "the first pin failed"
grep -q 'version *= "3.2.3"' .terraform.lock.hcl || fail "pin didn't lock hashicorp/null 3.2.3: $(cat .terraform.lock.hcl)"

echo "--- 2. build, recording MODULE.bazel.lock ---" >&2
"${BAZEL}" build //:versions >&2 || fail "the build against the first lock failed"
cp MODULE.bazel.lock "${_TMPDIR}/MODULE.bazel.lock.before"

echo "--- 3. bump hashicorp/null as a bot would ---" >&2
sed -i.bak 's/version = "3.2.3"/version = "3.2.4"/' versions.tf
rm versions.tf.bak
cp bumped.lock.hcl .terraform.lock.hcl

echo "--- 4. build with --lockfile_mode=error ---" >&2
"${BAZEL}" build --lockfile_mode=error //:versions >&2 ||
  fail "the build after a lock-only bump failed; does MODULE.bazel.lock depend on the provider version?"
cmp MODULE.bazel.lock "${_TMPDIR}/MODULE.bazel.lock.before" >&2 ||
  fail "MODULE.bazel.lock changed after a lock-only bump"

echo "OK: the first pin worked, and a lock-only bump needed no MODULE.bazel.lock change" >&2
