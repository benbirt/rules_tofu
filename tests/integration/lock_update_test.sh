#!/usr/bin/env bash
# Driver for //tests/integration:lock_update_test. The bazel_integration_test
# rule sets BIT_BAZEL_BINARY and BIT_WORKSPACE_DIR (the lock_update
# workspace). Unlike assert_bazel_fails.sh, every step here must succeed:
#
#   1. `bazel run @tf_providers//:pin` writes the first lock, although the
#      workspace's libraries already name `@tf_providers//hashicorp/null`,
#      which has no package in the hub until it is locked. The package's two
#      libraries each require the provider, in a file of their own, and the
#      lock meets both constraints (issue #99).
#   2. A build records MODULE.bazel.lock.
#   3. hashicorp/null is bumped the way Dependabot or Renovate does it: in
#      versions.tf and .terraform.lock.hcl, and nowhere else.
#   4. The build passes with --lockfile_mode=error and leaves MODULE.bazel.lock
#      byte-identical, so the bot's change is complete on its own.
#   5. A root `tf_providers.provider` tag for hashicorp/null, still in the
#      lock, fails the build naming the conflict, in the provider's fetch and
#      not in the extension.
#   6. So `pin` still runs: it leaves the tagged provider out, and with
#      nothing else to lock writes an empty lock.
#   7. The tag alone pins the provider, and the build passes.

set -euo pipefail

_TMPDIR="${TEST_TMPDIR:-/tmp}"

# The nested Bazel's caches and the `bazel` function that runs it.
# shellcheck source=tests/integration/nested_bazel.sh
source "${BIT_WORKSPACE_DIR}/../nested_bazel.sh"

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

# pin runs `bazel query` itself, so it gets the same options through $BAZEL.
BAZEL="${_TMPDIR}/bazel"
cat >"${BAZEL}" <<EOF
#!/usr/bin/env bash
exec "${BIT_BAZEL_BINARY}" --output_user_root="${NESTED_USER_ROOT}" --output_base="${NESTED_OUTPUT_BASE}" "\$@"
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
constraints="$(grep 'constraints *=' .terraform.lock.hcl)"
[[ "${constraints}" == *'3.2.3'* && "${constraints}" == *'>= 3.0'* ]] ||
  fail "pin didn't lock against both libraries' constraints (versions.tf's 3.2.3 and floor.tf's >= 3.0): ${constraints}"

echo "--- 2. build, recording MODULE.bazel.lock ---" >&2
"${BAZEL}" build //:versions //:floor >&2 || fail "the build against the first lock failed"
cp MODULE.bazel.lock "${_TMPDIR}/MODULE.bazel.lock.before"

echo "--- 3. bump hashicorp/null as a bot would ---" >&2
sed -i.bak 's/version = "3.2.3"/version = "3.2.4"/' versions.tf
rm versions.tf.bak
cp bumped.lock.hcl .terraform.lock.hcl

echo "--- 4. build with --lockfile_mode=error ---" >&2
"${BAZEL}" build --lockfile_mode=error //:versions //:floor >&2 ||
  fail "the build after a lock-only bump failed; does MODULE.bazel.lock depend on the provider version?"
cmp MODULE.bazel.lock "${_TMPDIR}/MODULE.bazel.lock.before" >&2 ||
  fail "MODULE.bazel.lock changed after a lock-only bump"

echo "--- 5. a root tag for the locked provider fails the build, not the extension ---" >&2
cat >>MODULE.bazel <<'EOF'
tf_providers.provider(
    source = "hashicorp/null",
    version = "3.2.4",
)
EOF
if out="$("${BAZEL}" build //:versions 2>&1)"; then
  fail "the build passed with hashicorp/null both tagged and locked"
fi
echo "${out}" >&2
grep -q -F 'is pinned both by a tf_providers.provider tag' <<<"${out}" ||
  fail "the failure doesn't name the conflict"

echo "--- 6. pin still runs, and leaves the tagged provider out of the lock ---" >&2
"${BAZEL}" run @tf_providers//:pin >&2 || fail "pin failed with a provider both tagged and locked"
cat .terraform.lock.hcl >&2
if grep -q 'hashicorp/null' .terraform.lock.hcl; then
  fail "the lock still pins hashicorp/null after pin"
fi

echo "--- 7. the tag alone pins the provider ---" >&2
"${BAZEL}" build //:versions //:floor >&2 || fail "the build with the provider tagged failed"

echo "OK: the first pin worked, a lock-only bump needed no MODULE.bazel.lock change, and a tag conflict left pin runnable" >&2
