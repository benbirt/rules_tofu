#!/usr/bin/env bash
# Driver for //tests/integration:module_dependency_*_test. The
# bazel_integration_test rule sets BIT_BAZEL_BINARY and BIT_WORKSPACE_DIR (the
# module_dependency sub-workspace), and the test sets TOFU_VERSION.
#
# Steps:
#   1. `bazel build //...` validates //app (which calls @network_module//vpc by
#      module_source, which in turn calls @dns_module//:dns) and //app:app_test.
#      It also builds the empty-root libraries (//empty), the `tofu show -json
#      -module` call fixture (//reader, covering variable defaults, local
#      values, _override files, .tofu shadowing and .tf.json; issue #153), the
#      library whose srcs are all in a subdirectory (//sub_only) and the deploy
#      without srcs (//nested:no_srcs_deploy).
#   2. @network_module//vpc and @dns_module//:dns also build on their own, as
#      root modules in their own repositories.
#   3. `bazel run //app:app.plan` plans offline against the generated module
#      manifest and checks the module chain's output.
#   4. From app/, `bazel run @rules_tofu//tofu -- version` reports TOFU_VERSION,
#      catching a version override that did not take effect. A second run passes
#      `fmt -list=false -write=false -diff` a relative file path and checks the
#      diff, verifying that tofu runs in the directory Bazel was started in.
#   5. `bazel test //app:app_test` runs the deploy's tf_test, whose run block
#      loads a module from the manifest, and checks that tofu-test.json is
#      written to its test outputs. The runner passes `tofu test` a flag
#      (-json-into) that OpenTofu before 1.12 lacks, and tf_test once ran on no
#      version except the default (issue #125). The step also checks that
#      app_test's runfiles manifest lists app_test.validate.stamp, ordering
#      validation before tofu test (issue #143). `bazel test //empty:test` runs
#      a tf_test of an empty root module whose run block loads the library
#      beside it (issue #119). `bazel test //provisioner:test` applies a module
#      whose local-exec provisioner runs a script from data that writes beside
#      itself in the run directory. Running `bazel-bin/provisioner/test.sh`
#      directly by a relative path exercises the same test without Bazel's test
#      environment. Neither run writes into the source tree.
#   6. `bazel run //app:app.apply` applies a deploy with no backend block and
#      allow_ephemeral_state = True, writing terraform.tfstate in the run
#      directory and leaving no .terraform/terraform.tfstate backend record.
#   7. `bazel run //app:durable.apply` checks a deploy whose backend "local"
#      paths come from backend_override.tf and var.state_root without
#      allow_ephemeral_state: apply is refused while state_root is relative and
#      succeeds once state_root is rewritten to an absolute path outside
#      bazel-out.
#   8. Run the README's formatting commands at the workspace root; rules_tofu
#      generates no format targets, so these commands are how a workspace is
#      formatted. `bazel run @rules_tofu//tofu -- fmt -check -diff -recursive .`
#      passes on the workspace as it is, with the bazel-* links present. After
#      writing one unformatted .tf, .tofu, .tfvars, .tftest.hcl and
#      .tofutest.hcl file at several depths, `fmt -check -recursive .` fails and
#      lists exactly those files. `fmt -recursive .` rewrites them and the check
#      passes again. Unformatted files behind a directory symlink, in a
#      dot-directory or with a dot-prefixed name are neither reported nor
#      rewritten.
#   9. `bazel build //reader:providers` fails strict deps and names the exact
#      providers OpenTofu reports for reader/providers/main.tf: canonical
#      addresses, resource- and data-source-implied providers, aliased provider
#      blocks reported once, and the built-in provider skipped (issue #153).
#
# Runs once per OpenTofu version in KNOWN_VERSIONS because the module manifest
# and the .terraform/terraform.tfstate backend record read by the
# ephemeral-state check are internal OpenTofu formats, and `tofu show -json
# -module` carries no format_version.

set -euo pipefail

_TMPDIR="${TEST_TMPDIR:-/tmp}"

# The nested Bazel's caches and the `bazel` function that runs it.
# shellcheck source=tests/integration/nested_bazel.sh
source "${BIT_WORKSPACE_DIR}/../nested_bazel.sh"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

[[ -n "${TOFU_VERSION:-}" ]] || fail "TOFU_VERSION is not set"

# The workspace sits among read-only runfiles, so work on a copy whose root
# selects TOFU_VERSION, pointing its local_path_override at the rules_tofu
# sources beside the original.
RULES_TOFU="$(cd "${BIT_WORKSPACE_DIR}/../../.." && pwd)"
WS="${_TMPDIR}/module_dependency"
rm -rf "${WS}"
cp -RL "${BIT_WORKSPACE_DIR}" "${WS}"
chmod -R u+w "${WS}"
sed -i.bak "s|path = \"../../..\"|path = \"${RULES_TOFU}\"|" "${WS}/MODULE.bazel"
rm "${WS}/MODULE.bazel.bak"
echo "tofu.version(version = \"${TOFU_VERSION}\")" >>"${WS}/MODULE.bazel"
cd "${WS}"

echo "--- 1. build //... ---" >&2
bazel build //... >&2 || fail "building the workspace failed"

echo "--- 2. build the libraries in their own repositories ---" >&2
bazel build @network_module//vpc @dns_module//:dns >&2 || fail "building the external libraries failed"

echo "--- 3. plan ---" >&2
out="$(bazel run //app:app.plan 2>&1)" || fail "the plan failed: ${out}"
echo "${out}" >&2
grep -q -F 'www.app.example.com' <<<"${out}" || fail "the plan doesn't show the module chain's output"

echo "--- 4. run the pinned tofu ---" >&2
version="$(cd app && bazel run @rules_tofu//tofu -- version)" || fail "bazel run @rules_tofu//tofu failed"
grep -q -x -F "OpenTofu v${TOFU_VERSION}" <<<"${version}" || fail "expected tofu ${TOFU_VERSION}, got: ${version}"
# A relative path resolves only if tofu runs in the directory Bazel started in.
printf 'variable   "unformatted" {}\n' >app/scratch.tf
out="$(cd app && bazel run @rules_tofu//tofu -- fmt -list=false -write=false -diff scratch.tf)" || fail "tofu fmt of a file in the directory Bazel was started in failed"
grep -q -x -F '+variable "unformatted" {}' <<<"${out}" || fail "tofu fmt did not print the diff of app/scratch.tf: ${out}"
rm app/scratch.tf

echo "--- 5. run the tf_test ---" >&2
bazel test //app:app_test --test_output=all >&2 || fail "the tf_test failed"
outputs_dir="$(bazel info bazel-testlogs)/app/app_test/test.outputs"
# Bazel zips a test's undeclared outputs unless told not to.
json_log=""
if [[ -f "${outputs_dir}/tofu-test.json" ]]; then
  json_log="$(cat "${outputs_dir}/tofu-test.json")"
elif [[ -f "${outputs_dir}/outputs.zip" ]]; then
  json_log="$(unzip -p "${outputs_dir}/outputs.zip" tofu-test.json 2>/dev/null || true)"
fi
grep -q -F '"@message"' <<<"${json_log}" || fail "the test left no JSON log (tofu-test.json) in ${outputs_dir}"
grep -q -F 'app/app_test.validate.stamp' "$(bazel info bazel-bin)/app/app_test.sh.runfiles_manifest" ||
  fail "app_test.sh.runfiles_manifest lacks app/app_test.validate.stamp"
# //empty:test has an empty root module; its run block loads ../nested.
bazel test //empty:test --test_output=errors >&2 || fail "the test of an empty root module failed"
# hello.sh runs through its symlink and writes ran.txt in the run directory.
bazel test //provisioner:test --test_output=errors >&2 || fail "the test of a module with a provisioner script failed"
# Unset this driver's Bazel test variables so the launcher resolves $0.runfiles
# from its own relative path.
env -u TEST_SRCDIR -u TEST_TMPDIR -u TEST_UNDECLARED_OUTPUTS_DIR bazel-bin/provisioner/test.sh >&2 || fail "the tf_test launcher failed when run directly by a relative path"
[[ ! -e provisioner/ran.txt ]] || fail "the provisioner script wrote into the source tree"

echo "--- 6. apply with no backend block ---" >&2
out="$(bazel run //app:app.apply -- -auto-approve 2>&1)" || fail "the apply with no backend block failed: ${out}"
echo "${out}" >&2
RUN_DIR="$(bazel info bazel-bin)/app/app.rules_tofu-state/work/app"
grep -q -F 'www.app.example.com' "${RUN_DIR}/terraform.tfstate" || fail "no state at ${RUN_DIR}/terraform.tfstate"
[[ ! -e "${RUN_DIR}/.terraform/terraform.tfstate" ]] || fail "tofu init recorded a backend for a deploy with no backend block: $(cat "${RUN_DIR}/.terraform/terraform.tfstate")"

echo "--- 7. the ephemeral-state gate reads the backend tofu init recorded ---" >&2
if out="$(bazel run //app:durable.apply -- -auto-approve 2>&1)"; then
  fail "the apply ran against a backend \"local\" with relative paths: ${out}"
fi
echo "${out}" >&2
grep -q -F 'its `path` ("state/app.tfstate") is relative' <<<"${out}" || fail "the refusal doesn't name the path tofu resolved"

STATE_ROOT="${_TMPDIR}/durable_state"
sed -i.bak "s|\"state_root\": \"state\"|\"state_root\": \"${STATE_ROOT}\"|" app/BUILD.bazel
rm app/BUILD.bazel.bak
out="$(bazel run //app:durable.apply -- -auto-approve 2>&1)" || fail "the apply against a backend \"local\" outside Bazel's output tree failed: ${out}"
echo "${out}" >&2
grep -q -F 'www.app.example.com' "${STATE_ROOT}/app.tfstate" || fail "no state at ${STATE_ROOT}/app.tfstate"

echo "--- 8. format the workspace ---" >&2
# The builds above created the bazel-* directory symlinks; -recursive must
# skip them.
[[ -L bazel-bin ]] || fail "the workspace has no bazel-bin link for tofu fmt to skip"
bazel run @rules_tofu//tofu -- fmt -check -diff -recursive . >&2 || fail "tofu fmt -check failed on the formatted workspace"

unformatted='variable   "unformatted" {}'
# One unformatted file of each kind tofu fmt handles, at several depths.
fmt_files=(scratch.tf app/scratch.tofu app/scratch.tfvars app/tests/scratch.tftest.hcl nested/sub/scratch.tofutest.hcl)
for f in "${fmt_files[@]}"; do
  echo "${unformatted}" >"${f}"
done
# Files behind a directory symlink, in a dot-directory or with a dot-prefixed
# name must be skipped.
mkdir "${_TMPDIR}/linked" .hidden
echo "${unformatted}" >"${_TMPDIR}/linked/scratch.tf"
ln -s "${_TMPDIR}/linked" linked
echo "${unformatted}" >.hidden/scratch.tf
echo "${unformatted}" >app/.scratch.tf

if out="$(bazel run @rules_tofu//tofu -- fmt -check -recursive . 2>/dev/null)"; then
  fail "tofu fmt -check passed on unformatted files: ${out}"
fi
echo "${out}" >&2
[[ "$(sort <<<"${out}")" == "$(printf '%s\n' "${fmt_files[@]}" | sort)" ]] || fail "tofu fmt -check should name exactly ${fmt_files[*]}, got: ${out}"

bazel run @rules_tofu//tofu -- fmt -recursive . >&2 || fail "tofu fmt failed"
bazel run @rules_tofu//tofu -- fmt -check -diff -recursive . >&2 || fail "tofu fmt -check still fails after tofu fmt"
for f in "${fmt_files[@]}"; do
  grep -q -x -F 'variable "unformatted" {}' "${f}" || fail "tofu fmt didn't rewrite ${f}: $(cat "${f}")"
done
for f in linked/scratch.tf .hidden/scratch.tf app/.scratch.tf; do
  grep -q -x -F "${unformatted}" "${f}" || fail "tofu fmt rewrote ${f}, which -recursive should not reach"
done
rm -r "${fmt_files[@]}" linked .hidden app/.scratch.tf "${_TMPDIR}/linked"

echo "--- 9. the providers tofu reports for a module ---" >&2
if out="$(bazel build //reader:providers 2>&1)"; then
  fail "//reader:providers built without the providers its files require: ${out}"
fi
echo "${out}" >&2
want='rules_tofu: //reader:providers: its files require these providers, which are not in its providers:
    example.com/acme/corp, required in "reader/providers"
    hashicorp/google, required in "reader/providers"
    hashicorp/http, required in "reader/providers"
    hashicorp/null, required in "reader/providers"
    hashicorp/random, required in "reader/providers"
Add "@tf_providers//hashicorp/google", "@tf_providers//hashicorp/http", "@tf_providers//hashicorp/null", "@tf_providers//hashicorp/random" to the providers of //reader:providers.'
grep -q -F "${want}" <<<"${out}" || fail "the strict-deps error doesn't name exactly the providers of reader/providers/main.tf"

echo "OK: with tofu ${TOFU_VERSION}, modules from other Bazel modules were called by module_source, and tofu fmt formatted the workspace" >&2
