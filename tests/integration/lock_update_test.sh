#!/usr/bin/env bash
# Driver for //tests/integration:lock_update_test. The bazel_integration_test
# rule sets BIT_BAZEL_BINARY and BIT_WORKSPACE_DIR (the lock_update
# sub-workspace).
#
# Steps:
#   1. `bazel run @tf_providers//:pin` writes the initial
#      infra/.terraform.lock.hcl although the libraries already name
#      @tf_providers//hashicorp/null, which has no package in the hub until it
#      is locked. The lock meets the constraints of both root-package libraries
#      (versions.tf and floor.tf, issue #99) and of a submodule in
#      @bundle_module//bundle, a module_source bundle with no configuration in
#      its own directory (issue #123).
#   2. Building //:versions, //:floor and //empty:lib writes MODULE.bazel.lock.
#      //empty:lib lists the locked provider and has no files; it validates
#      because an empty root module is not given the lock (issue #119).
#   3. Bump hashicorp/null as Dependabot or Renovate does: in versions.tf and
#      infra/.terraform.lock.hcl, and nowhere else.
#   4. Building //:versions and //:floor passes with --lockfile_mode=error and
#      leaves MODULE.bazel.lock byte-identical.
#   5. In a directory with only the files Dependabot's bazel updater fetches,
#      `bazel mod tidy --lockfile_mode=update` regenerates MODULE.bazel.lock
#      and leaves it and MODULE.bazel byte-identical (issue #131).
#   6. Adding a root tf_providers.provider tag for hashicorp/null while it is
#      still in infra/.terraform.lock.hcl fails `bazel build //:versions` in
#      the provider's fetch (not in the extension), naming the conflict.
#   7. Because the extension still evaluates, pin remains runnable: it omits
#      the tagged provider and writes an empty lock.
#   8. Building //:versions and //:floor passes with the root tag alone.

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
[[ ! -e infra/.terraform.lock.hcl ]] || fail "the workspace already has a lock"
"${BAZEL}" run @tf_providers//:pin >&2 || fail "the first pin failed"
grep -q 'version *= "3.2.3"' infra/.terraform.lock.hcl || fail "pin didn't lock hashicorp/null 3.2.3: $(cat infra/.terraform.lock.hcl)"
constraints="$(grep 'constraints *=' infra/.terraform.lock.hcl)"
[[ "${constraints}" == *'3.2.3'* && "${constraints}" == *'>= 3.0'* ]] ||
  fail "pin didn't lock against both libraries' constraints (versions.tf's 3.2.3 and floor.tf's >= 3.0): ${constraints}"
[[ "${constraints}" == *'>= 3.1'* ]] ||
  fail "pin left out the constraint of the bundle library's submodule that floor.tf calls (>= 3.1): ${constraints}"

echo "--- 2. build, recording MODULE.bazel.lock ---" >&2
"${BAZEL}" build //:versions //:floor //empty:lib >&2 || fail "the build against the first lock failed"
cp MODULE.bazel.lock "${_TMPDIR}/MODULE.bazel.lock.before"

echo "--- 3. bump hashicorp/null as a bot would ---" >&2
sed -i.bak 's/version = "3.2.3"/version = "3.2.4"/' versions.tf
rm versions.tf.bak
cp bumped.lock.hcl infra/.terraform.lock.hcl

echo "--- 4. build with --lockfile_mode=error ---" >&2
"${BAZEL}" build --lockfile_mode=error //:versions //:floor >&2 ||
  fail "the build after a lock-only bump failed; does MODULE.bazel.lock depend on the provider version?"
cmp MODULE.bazel.lock "${_TMPDIR}/MODULE.bazel.lock.before" >&2 ||
  fail "MODULE.bazel.lock changed after a lock-only bump"

echo "--- 5. Dependabot's bazel updater, with only the files it fetches ---" >&2
# Dependabot does not clone the repository. Its bazel file fetcher takes the
# root Bazel files, local_path_override directories, and files MODULE.bazel
# names by text match in attributes it knows, each with the BUILD file beside
# it. lock_file is the attribute from_lock uses for that match. Dependabot then
# runs `bazel mod tidy` to regenerate MODULE.bazel.lock. That runs from_lock,
# so the lock and its package must be among the fetched files (issue #131).
DEPENDABOT="${_TMPDIR}/dependabot"
rm -rf "${DEPENDABOT}"
mkdir "${DEPENDABOT}"
for f in MODULE.bazel MODULE.bazel.lock .bazelversion .bazelrc BUILD BUILD.bazel; do
  [[ ! -e "${f}" ]] || cp "${f}" "${DEPENDABOT}/${f}"
done
# The match and the label-to-path conversion are Dependabot's
# (module_path_extractor.rb and path_converter.rb in dependabot-core).
labels="$(grep -o -E 'lock_file[[:space:]]*=[[:space:]]*"[^"]+"' MODULE.bazel | sed -E 's/.*"([^"]+)"$/\1/')" ||
  fail "MODULE.bazel names no lock_file for Dependabot to fetch"
while IFS= read -r label; do
  path="$(sed -E -e 's|^@[^/]+//||' -e 's|:|/|g' -e 's|^/+||' <<<"${label}")"
  dir="$(dirname "${path}")"
  mkdir -p "${DEPENDABOT}/${dir}"
  for f in "${path}" "${dir}/BUILD" "${dir}/BUILD.bazel"; do
    [[ ! -e "${f}" ]] || cp "${f}" "${DEPENDABOT}/${f}"
  done
done <<<"${labels}"
# local_path_override's other path is the absolute one to rules_tofu, which
# Dependabot leaves out; in a real workspace that is a registry dependency.
cp -R bundle_module "${DEPENDABOT}/bundle_module"
(
  cd "${DEPENDABOT}"
  "${BIT_BAZEL_BINARY}" --output_user_root="${NESTED_USER_ROOT}" --output_base="${_TMPDIR}/dependabot_output_base" \
    mod tidy --lockfile_mode=update >&2 || fail "bazel mod tidy failed with only the files Dependabot fetches"
  "${BIT_BAZEL_BINARY}" --output_user_root="${NESTED_USER_ROOT}" --output_base="${_TMPDIR}/dependabot_output_base" \
    shutdown
)
cmp MODULE.bazel "${DEPENDABOT}/MODULE.bazel" >&2 || fail "bazel mod tidy changed MODULE.bazel"
cmp MODULE.bazel.lock "${DEPENDABOT}/MODULE.bazel.lock" >&2 || fail "bazel mod tidy changed MODULE.bazel.lock"

echo "--- 6.a root tag for the locked provider fails the build, not the extension ---" >&2
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

echo "--- 7. pin still runs, and leaves the tagged provider out of the lock ---" >&2
"${BAZEL}" run @tf_providers//:pin >&2 || fail "pin failed with a provider both tagged and locked"
cat infra/.terraform.lock.hcl >&2
if grep -q 'hashicorp/null' infra/.terraform.lock.hcl; then
  fail "the lock still pins hashicorp/null after pin"
fi

echo "--- 8. the tag alone pins the provider ---" >&2
"${BAZEL}" build //:versions //:floor >&2 || fail "the build with the provider tagged failed"

echo "OK: the first pin worked, a lock-only bump needed no MODULE.bazel.lock change, Dependabot's files were enough for it, and a tag conflict left pin runnable" >&2
