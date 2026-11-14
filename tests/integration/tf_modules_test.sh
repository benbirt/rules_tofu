#!/usr/bin/env bash
# Driver for //tests/integration:tf_modules_test. The bazel_integration_test
# rule sets BIT_BAZEL_BINARY and BIT_WORKSPACE_DIR (the tf_modules
# sub-workspace).
#
# Steps:
#   1. Turn fixtures/network into a git repository tagged v1.0.0 and
#      fixtures/dns into dns.tar.gz, then fill in @FIXTURES@ and @DNS_SHA256@.
#      In network.git:
#      - vpc calls its own //vpc/tags submodule by its full git URL, which the
#        module provides itself (issue #95).
#      - Files named `files` at the root and `module` in vpc/ check against
#        target-name collisions (issue #61), and vpc/examples/BUILD.bazel
#        checks that upstream BUILD files are removed.
#      - Symlinks cover every kind: to a file the module needs
#        (vpc/modules/subnet/outputs.tf), to a directory, to an ancestor
#        (which Bazel's glob cannot follow), to nothing, out of the repository,
#        and peering_link at the root pointing to vpc/peering.
#      - terraform.tfvars and vpc/examples/complete/demo.auto.tfvars would fail
#        every build if generated libraries included auto-loaded variable files
#        in data.
#      - .bazelignore naming vpc/modules would hide the subnet submodule if
#        left in the fetched repository (issue #102).
#      - An uninitialised git submodule entry at vendor/shared is unused and
#        not fetched (issue #94).
#   2. //app plans offline across the git module (which calls a registry module
#      and requires a provider only the module names), a submodule under
#      modules/, one outside modules/ via `//<subdir>`, the archive, and the
#      registry module directly. Checks the fetched network repository: the
#      file symlink and the files named `files` and `module` remain, while
#      .bazelignore, BUILD.bazel and all other symlinks were removed.
#   3. Removing hashicorp/time from //app's providers fails `bazel build //app`
#      naming the label to add: network's main library cannot carry providers
#      of submodules outside modules/ that only callers reach (issue #76).
#   4. MODULE.bazel.lock records the git commit and registry module metadata.
#      Replacing the git module's fact with an archive-style fact, shutting
#      down Bazel and removing the output base's lockfile makes the next build
#      resolve and record the commit again (issue #96).
#   5. Moving v1.0.0 to a new commit and running `bazel clean --expunge` still
#      plans the old commit's content and passes with --lockfile_mode=error.
#   6. Switching from provider tags to tf_providers.from_lock() and running
#      @tf_providers//:pin locks hashicorp/null (from the git module) and
#      hashicorp/time (from //vpc/peering), leaves out hashicorp/random (used
#      only under examples/), and re-plans //app.
#   7. Building //bad, which calls a fetched module that calls an undeclared
#      one, fails during fetch naming the tf_modules.module tag to add.
#   8. Declaring a module by a `//<subdir>` (peering_link) that is a symlink
#      fails during fetch naming the directory the link points to.
#   9. Declaring a module by a `//<subdir>` (vendor/shared) inside a git
#      submodule fails during fetch naming the submodule.

set -euo pipefail

_TMPDIR="${TEST_TMPDIR:-/tmp}"

# The nested Bazel's caches and the `bazel` function that runs it.
# shellcheck source=tests/integration/nested_bazel.sh
source "${BIT_WORKSPACE_DIR}/../nested_bazel.sh"

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
# The module's call to its own address must name this repository.
sed -i.bak "s|@FIXTURES@|${FIXTURES}|g" "${FIXTURES}/network.git/vpc/main.tf"
rm "${FIXTURES}/network.git/vpc/main.tf.bak"
grep -q -F "git::file://${FIXTURES}/network.git//vpc/tags?ref=v1.0.0" "${FIXTURES}/network.git/vpc/main.tf" ||
  fail "couldn't fill in the git module's call to its own address"
# Files named like the generated targets, a Bazel file, and symlinks. The
# subnet submodule's outputs come through the one symlink that must survive;
# `cp -RL` above would have dereferenced them, so they're made here.
echo "not a target" >"${FIXTURES}/network.git/files"
echo "not a target" >"${FIXTURES}/network.git/vpc/module"
echo 'filegroup(name = "upstream")' >"${FIXTURES}/network.git/vpc/examples/BUILD.bazel"
ln -s ../../../common/subnet_outputs.tf "${FIXTURES}/network.git/vpc/modules/subnet/outputs.tf"
ln -s ../../modules "${FIXTURES}/network.git/vpc/examples/complete/modules"
ln -s ../.. "${FIXTURES}/network.git/vpc/examples/complete/parent"
ln -s ../../../missing.tf "${FIXTURES}/network.git/vpc/examples/complete/missing.tf"
ln -s /dev/null "${FIXTURES}/network.git/vpc/examples/complete/outside"
ln -s vpc/peering "${FIXTURES}/network.git/peering_link"
# Variables files that `tofu` loads by itself, as a module's root and its
# examples often carry. No rule accepts one as data, so the generated
# libraries must leave them out: every build below would fail otherwise.
echo 'name = "upstream"' >"${FIXTURES}/network.git/terraform.tfvars"
echo 'name = "example"' >"${FIXTURES}/network.git/vpc/examples/complete/demo.auto.tfvars"
# Bazel reads a .bazelignore at the root of any repository: left in the
# fetched module, this one would hide the subnet submodule from its library.
echo "vpc/modules" >"${FIXTURES}/network.git/.bazelignore"
git_ init -q
git_ add -A
# A git submodule, as the index records one: a commit of another repository
# at a path. There is nothing to fetch it from, and nothing fetches it. Its
# directory exists and is empty, as in a checkout that hasn't initialised
# it, so that `commit -a` below finds it unchanged.
mkdir -p "${FIXTURES}/network.git/vendor/shared"
git_ update-index --add --cacheinfo 160000,1111111111111111111111111111111111111111,vendor/shared
git_ commit -q -m v1
git_ tag -a -m v1.0.0 v1.0.0
first_commit="$(git_ rev-parse HEAD)"
tar -czf "${FIXTURES}/dns.tar.gz" -C "${FIXTURES}/dns" .
# macOS has shasum but not sha256sum; some Linux PATHs have only sha256sum.
if command -v sha256sum >/dev/null; then
  dns_sha256="$(sha256sum "${FIXTURES}/dns.tar.gz" | cut -d' ' -f1)"
else
  dns_sha256="$(shasum -a 256 "${FIXTURES}/dns.tar.gz" | cut -d' ' -f1)"
fi
for f in MODULE.bazel app/main.tf bad/main.tf; do
  sed -i.bak \
    -e "s|path = \"../../..\"|path = \"${RULES_TOFU}\"|" \
    -e "s|@FIXTURES@|${FIXTURES}|g" \
    -e "s|@DNS_SHA256@|${dns_sha256}|g" \
    "${WS}/${f}"
  rm "${WS}/${f}.bak"
done
cd "${WS}"

plan() {
  local out
  out="$(bazel run //app:app.plan 2>&1)" || fail "the plan failed: ${out}"
  echo "${out}" >&2
  grep -q -F "$1" <<<"${out}" || fail "the plan doesn't show '$1'"
}

echo "--- 2. plan ---" >&2
plan "app-v1 10.0.0.0/24 www.example.com direct hub-peered"
network_repo="$(find "$(bazel info output_base)/external" -maxdepth 1 -name '*tf_modules_network')"
[[ -d "${network_repo}" ]] || fail "can't find the network module's repository: '${network_repo}'"
[[ -L "${network_repo}/vpc/modules/subnet/outputs.tf" ]] || fail "the symlink to a file of the module was removed"
for removed in .bazelignore vpc/examples/BUILD.bazel vpc/examples/complete/modules vpc/examples/complete/parent vpc/examples/complete/missing.tf vpc/examples/complete/outside peering_link; do
  if [[ -e "${network_repo}/${removed}" || -L "${network_repo}/${removed}" ]]; then
    fail "${removed} is still in the fetched module"
  fi
done
[[ -f "${network_repo}/files" && -f "${network_repo}/vpc/module" ]] || fail "the module's own files named files and module were removed"

echo "--- 3. a submodule's provider missing from the caller ---" >&2
# //vpc/peering requires hashicorp/time, which only //app lists: the network
# module's library can't know which submodules outside modules/ callers reach.
cp app/BUILD.bazel "${_TMPDIR}/app.BUILD.bazel"
sed -i.bak '/providers = \["@tf_providers\/\/hashicorp\/time"\],/d' app/BUILD.bazel
rm app/BUILD.bazel.bak
if grep -q 'providers = ' app/BUILD.bazel; then
  fail "couldn't remove hashicorp/time from //app's providers"
fi
if out="$(bazel build //app 2>&1)"; then
  fail "//app built without hashicorp/time in its providers, which //vpc/peering requires"
fi
echo "${out}" >&2
# The dep's label is the per-module repository's canonical one.
grep -q -F '//vpc:_rules_tofu_module, //vpc/peering) requires hashicorp/time' <<<"${out}" ||
  fail "the failure doesn't name the submodule and the provider it requires"
grep -q -F 'Add "@tf_providers//hashicorp/time" to the providers of //app:app.' <<<"${out}" ||
  fail "the failure doesn't name the label to add"
mv "${_TMPDIR}/app.BUILD.bazel" app/BUILD.bazel

echo "--- 4. facts ---" >&2
grep -q -F "\"commit\": \"${first_commit}\"" MODULE.bazel.lock || fail "MODULE.bazel.lock doesn't record ${first_commit}"
grep -q -F '"cloudposse/label/null@0.25.0"' MODULE.bazel.lock || fail "MODULE.bazel.lock doesn't record the registry module"
# A badly merged lockfile: under the git module's key, a well-formed fact of
# the kind a registry module with an archive has. It is no use there, so it
# must be resolved again (from the fixture, offline), not trusted or tripped
# over.
sed -i.bak "s|\"commit\": \"${first_commit}\"|\"location\": \"https://example.com/x.zip\", \"sha256\": \"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855\"|" MODULE.bazel.lock
rm MODULE.bazel.lock.bak
if grep -q -F "\"commit\": \"${first_commit}\"" MODULE.bazel.lock; then
  fail "couldn't replace the git module's fact in MODULE.bazel.lock"
fi
# As on another machine: a server that has yet to evaluate the extension,
# and none of the facts Bazel keeps for itself in the output base's own
# lockfile, which would stand in for the bad one.
bazel shutdown >&2
rm -f "${NESTED_OUTPUT_BASE}/MODULE.bazel.lock"
bazel build //app >&2 || fail "the build failed with a fact of the wrong kind in MODULE.bazel.lock"
grep -q -F "\"commit\": \"${first_commit}\"" MODULE.bazel.lock || fail "the git module's fact wasn't resolved again"
if grep -q -F 'https://example.com/x.zip' MODULE.bazel.lock; then
  fail "the fact of the wrong kind is still in MODULE.bazel.lock"
fi

echo "--- 5. move the tag ---" >&2
sed -i.bak 's/"v1"/"v2"/' "${FIXTURES}/network.git/shared/main.tf"
rm "${FIXTURES}/network.git/shared/main.tf.bak"
git_ commit -q -a -m v2
git_ tag -f -a -m v1.0.0 v1.0.0 >&2
bazel clean --expunge >&2
plan "app-v1 10.0.0.0/24"
bazel build --lockfile_mode=error //app >&2 || fail "the build with --lockfile_mode=error failed"

echo "--- 6. pin ---" >&2
# The root pins hashicorp/null and hashicorp/time with a lock instead of tags.
# Nothing in the main repository requires them: the git module requires null,
# and its submodule outside modules/ that //app calls requires time.
sed -i.bak '/^tf_providers.provider($/,/^)$/d' MODULE.bazel
rm MODULE.bazel.bak
# A backslash-newline, not `\n`, which BSD sed doesn't read as a newline.
sed -i.bak 's|^use_repo(tf_providers, "tf_providers")$|tf_providers.from_lock(lock_file = "//:.terraform.lock.hcl")\
use_repo(tf_providers, "tf_providers")|' MODULE.bazel
rm MODULE.bazel.bak
grep -q -x -F 'tf_providers.from_lock(lock_file = "//:.terraform.lock.hcl")' MODULE.bazel || fail "couldn't switch MODULE.bazel to from_lock"
BAZEL="${_TMPDIR}/bazel"
cat >"${BAZEL}" <<EOF
#!/usr/bin/env bash
exec "${BIT_BAZEL_BINARY}" --output_user_root="${NESTED_USER_ROOT}" --output_base="${NESTED_OUTPUT_BASE}" "\$@"
EOF
chmod +x "${BAZEL}"
BAZEL="${BAZEL}" bazel run @tf_providers//:pin >&2 || fail "pin failed"
cat .terraform.lock.hcl >&2
grep -q 'provider "registry.opentofu.org/hashicorp/null"' .terraform.lock.hcl ||
  fail "the lock doesn't cover hashicorp/null, which the git module requires"
grep -q 'provider "registry.opentofu.org/hashicorp/time"' .terraform.lock.hcl ||
  fail "the lock doesn't cover hashicorp/time, which the submodule //app calls by //vpc/peering requires"
if grep -q 'hashicorp/random' .terraform.lock.hcl; then
  fail "the lock covers hashicorp/random, which only the git module's examples require"
fi
plan "app-v1 10.0.0.0/24"

echo "--- 7. an undeclared module ---" >&2
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

echo "--- 8. a module directory that is a symlink ---" >&2
cat >>MODULE.bazel <<EOF
tf_modules.module(
    name = "peering_link",
    source = "git::file://${FIXTURES}/network.git//peering_link?ref=v1.0.0",
)
EOF
if out="$(bazel build @tf_modules//peering_link 2>&1)"; then
  fail "@tf_modules//peering_link built, but its directory is a symlink"
fi
echo "${out}" >&2
grep -q -F '`peering_link` is a symlink in the fetched files, to vpc/peering' <<<"${out}" ||
  fail "the failure doesn't name the directory the link leads to"

echo "--- 9. a module directory in a git submodule ---" >&2
git_ ls-tree HEAD vendor/shared | grep -q '^160000 commit' || fail "the fixture's git submodule didn't survive to the tagged commit"
cat >>MODULE.bazel <<EOF
tf_modules.module(
    name = "shared_submodule",
    source = "git::file://${FIXTURES}/network.git//vendor/shared?ref=v1.0.0",
)
EOF
if out="$(bazel build @tf_modules//shared_submodule 2>&1)"; then
  fail "@tf_modules//shared_submodule built, but its directory is a git submodule, which is not fetched"
fi
echo "${out}" >&2
grep -q -F '"/vendor/shared" is in the git submodule "vendor/shared" of the repository, and rules_tofu does not fetch git submodules' <<<"${out}" ||
  fail "the failure doesn't name the git submodule"

echo "OK: tf_modules fetched git, archive and registry modules, pinned by commit" >&2
