#!/usr/bin/env bash
# Driver for //tests/integration:tf_modules_test. The bazel_integration_test
# rule sets BIT_BAZEL_BINARY and BIT_WORKSPACE_DIR (the tf_modules workspace).
# Every step must succeed, the last by seeing the build fail as it should:
#
#   1. fixtures/network becomes a git repository with an annotated tag v1.0.0,
#      and fixtures/dns an archive; @FIXTURES@ and @DNS_SHA256@ are filled in.
#   2. //app plans: it calls a git module (which calls a registry module and
#      requires a provider that only the module names), a submodule of it by
#      `//<subdir>`, the archive, and the registry module directly.
#   3. MODULE.bazel.lock records the commits of the git and registry modules.
#   4. v1.0.0 is moved to a new commit. After `bazel clean --expunge`, //app
#      still plans the old commit's content, and the build passes with
#      --lockfile_mode=error.
#   5. With the provider locked rather than tagged, `@tf_providers//:pin`
#      covers what the git module requires, but not what its examples do.
#   6. A module that calls an undeclared one fails when fetched, naming the
#      tag to add.

set -euo pipefail

_TMPDIR="${TEST_TMPDIR:-/tmp}"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

RULES_TOFU="$(cd "${BIT_WORKSPACE_DIR}/../../.." && pwd)"
WS="${_TMPDIR}/tf_modules"
FIXTURES="${_TMPDIR}/fixtures"
rm -rf "${WS}" "${FIXTURES}"
cp -RL "${BIT_WORKSPACE_DIR}" "${WS}"
chmod -R u+w "${WS}"
mv "${WS}/fixtures" "${FIXTURES}"

git_() {
  git -C "${FIXTURES}/network.git" -c user.name=rules_tofu -c user.email=rules_tofu@example.com "$@"
}

echo "--- 1. fixtures ---" >&2
mv "${FIXTURES}/network" "${FIXTURES}/network.git"
git_ init -q
git_ add -A
git_ commit -q -m v1
git_ tag -a -m v1.0.0 v1.0.0
first_commit="$(git_ rev-parse HEAD)"
tar -czf "${FIXTURES}/dns.tar.gz" -C "${FIXTURES}/dns" .
dns_sha256="$(sha256sum "${FIXTURES}/dns.tar.gz" | cut -d' ' -f1)"
for f in MODULE.bazel app/main.tf bad/main.tf; do
  sed -i.bak \
    -e "s|path = \"../../..\"|path = \"${RULES_TOFU}\"|" \
    -e "s|@FIXTURES@|${FIXTURES}|g" \
    -e "s|@DNS_SHA256@|${dns_sha256}|g" \
    "${WS}/${f}"
  rm "${WS}/${f}.bak"
done
cd "${WS}"

# Its own output base, as in assert_bazel_fails.sh: the one in ~/.bazelrc is
# the outer Bazel's.
NESTED_OUTPUT_BASE="${_TMPDIR}/nested_output_base"
bazel() {
  "${BIT_BAZEL_BINARY}" --output_base="${NESTED_OUTPUT_BASE}" "$@"
}

plan() {
  local out
  out="$(bazel run //app:app.plan 2>&1)" || fail "the plan failed: ${out}"
  echo "${out}" >&2
  grep -q -F "$1" <<<"${out}" || fail "the plan doesn't show '$1'"
}

echo "--- 2. plan ---" >&2
plan "app-v1 10.0.0.0/24 www.example.com direct"

echo "--- 3. facts ---" >&2
grep -q -F "\"commit\": \"${first_commit}\"" MODULE.bazel.lock || fail "MODULE.bazel.lock doesn't record ${first_commit}"
grep -q -F '"cloudposse/label/null@0.25.0"' MODULE.bazel.lock || fail "MODULE.bazel.lock doesn't record the registry module"

echo "--- 4. move the tag ---" >&2
sed -i.bak 's/"v1"/"v2"/' "${FIXTURES}/network.git/shared/main.tf"
rm "${FIXTURES}/network.git/shared/main.tf.bak"
git_ commit -q -a -m v2
git_ tag -f -a -m v1.0.0 v1.0.0 >&2
bazel clean --expunge >&2
plan "app-v1 10.0.0.0/24"
bazel build --lockfile_mode=error //app >&2 || fail "the build with --lockfile_mode=error failed"

echo "--- 5. pin ---" >&2
# The root pins hashicorp/null with a lock instead of a tag. Nothing in the
# main repository requires it: only the git module does.
sed -i.bak '/^tf_providers.provider($/,/^)$/d' MODULE.bazel
rm MODULE.bazel.bak
sed -i.bak 's|^use_repo(tf_providers, "tf_providers")$|tf_providers.from_lock()\nuse_repo(tf_providers, "tf_providers")|' MODULE.bazel
rm MODULE.bazel.bak
grep -q '^tf_providers.from_lock()$' MODULE.bazel || fail "couldn't switch MODULE.bazel to from_lock"
BAZEL="${_TMPDIR}/bazel"
cat >"${BAZEL}" <<EOF
#!/usr/bin/env bash
exec "${BIT_BAZEL_BINARY}" --output_base="${NESTED_OUTPUT_BASE}" "\$@"
EOF
chmod +x "${BAZEL}"
BAZEL="${BAZEL}" bazel run @tf_providers//:pin >&2 || fail "pin failed"
cat .terraform.lock.hcl >&2
grep -q 'provider "registry.opentofu.org/hashicorp/null"' .terraform.lock.hcl ||
  fail "the lock doesn't cover hashicorp/null, which the git module requires"
if grep -q 'hashicorp/random' .terraform.lock.hcl; then
  fail "the lock covers hashicorp/random, which only the git module's examples require"
fi
plan "app-v1 10.0.0.0/24"

echo "--- 6. an undeclared module ---" >&2
cat >>MODULE.bazel <<EOF
tf_modules.module(
    name = "caller",
    source = "git::file://${FIXTURES}/network.git//caller?ref=v1.0.0",
)
EOF
if out="$(bazel build //bad 2>&1)"; then
  fail "//bad built, but it calls a module MODULE.bazel doesn't declare"
fi
echo "${out}" >&2
grep -q -F 'tf_modules.module(source = "acme/missing/null", version = "...")' <<<"${out}" ||
  fail "the failure doesn't name the tag to add"

echo "OK: tf_modules fetched git, archive and registry modules, pinned by commit" >&2
