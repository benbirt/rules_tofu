"""Build-time `tofu init -backend=false` + `tofu validate` for a materialized
work tree.

The deploy rule (and the library's auto-emitted validating deploy) call
`tf_init_validate(...)` to attach a validate stamp to the rule's outputs, so
`bazel build :foo` exercises validation in the build graph (with caching).

`-plugin-dir` is always passed (pointing at the sibling plugin tree that
`materialize_plugin_tree(...)` declares), so init is offline regardless of
whether `required_providers` is declared. With zero providers in scope the
plugin tree has no files, so the action falls back to an empty directory and
init is still a no-op for provider installation.

When providers come from `tf_providers.from_lock`, the workspace's lock is
copied next to the configuration (after the check that refuses a lock in
`srcs`/`data`), so init fails when the configuration's version constraints
no longer match it, and checks the vendored packages against its hashes.

Modules are never fetched either: `.terraform/modules/modules.json` (from
`modules_manifest` in work_tree.bzl) records where in the work tree each
module call's files are, and `-get=false` makes init fail rather than fetch
when a call's `source` or `version` no longer matches it.
"""

load("//toolchain:toolchain.bzl", "TOOLCHAIN_TYPE")

def tf_init_validate(ctx, work_tree_files, work_tree_root, package_dir, plugin_tree_root, modules_manifest, lock = None, test_dir = None):
    """Emit a build-time `tofu init -backend=false` + `tofu validate` action.

    Args:
      ctx: rule ctx. Must list `TOOLCHAIN_TYPE` in `toolchains`.
      work_tree_files: depset[File] of every materialized work tree file,
          plus the sibling plugin tree's provider binaries (read in place via
          `-plugin-dir`, so they must still be listed as action inputs).
      work_tree_root: string, exec-root-relative path to the work tree root,
          e.g. `<bin>/<pkg>/<name>.work`.
      package_dir: string, workspace-relative directory inside the work tree
          that tofu should cd into before running init/validate.
      plugin_tree_root: string, exec-root-relative path to the sibling plugin
          tree root, e.g. `<bin>/<pkg>/<name>.plugins`. Pinned via
          `-plugin-dir` so init runs offline.
      modules_manifest: File, the `.terraform/modules/modules.json` recording
          every module call (see `modules_manifest` in work_tree.bzl).
      lock: optional File, the `.terraform.lock.hcl` pinning the providers
          (see `provider_lock` in work_tree.bzl).
      test_dir: for a `tf_test`, the test files' directory relative to
          `package_dir`, passed to init and validate as `-test-directory` so
          the test files are checked too; "" when they are in `package_dir`
          itself, which tofu reads without being told. None for a rule that
          has no test files.

    Returns:
      File: a stamp file declared as an action output.
    """
    tofu = ctx.toolchains[TOOLCHAIN_TYPE].tofu
    stamp = ctx.actions.declare_file(ctx.label.name + ".validate.stamp")

    # tofu init writes `.terraform/` and `.terraform.lock.hcl` next to the
    # config. The materialized work tree directory in the sandbox contains
    # symlinks (Bazel-managed); to avoid any chance of conflicting with
    # sandbox read-only enforcement on input trees, copy the work tree into
    # a scratch dir under $TMPDIR (dereferencing symlinks) and run there.
    # Only the (small) config tree is copied — the provider binaries live in
    # the sibling plugin tree and are read in place via -plugin-dir, never
    # copied. The copy is removed when the script exits: only linux-sandbox
    # gives an action a fresh $TMPDIR, while the darwin sandbox and local
    # execution write to the host's and clean nothing up (issue #90).
    ctx.actions.run_shell(
        inputs = depset(direct = [tofu.binary, modules_manifest] + ([lock] if lock else []), transitive = [work_tree_files]),
        outputs = [stamp],
        command = """\
set -euo pipefail
TOFU=$1
# A binary in the root package has a bare path like `tofu.sh`, which bash would
# look up in PATH instead of the exec root. Make it relative to here.
case "$TOFU" in */*) ;; *) TOFU="./$TOFU" ;; esac
WORK_TREE_ROOT=$2
PACKAGE_DIR=$3
PLUGIN_TREE_ROOT=$4
STAMP=$5
MODULES=$6
LOCK=${7:-}
TEST_DIR=${8:-}
# The test files' directory, for a tf_test: `.` when it is the root module's
# own, which tofu reads anyway, with its default, `tests`. A string, not an
# array: an empty array fails under `set -u` in bash 3.
TEST_FLAG=""
if [ -n "$TEST_DIR" ] && [ "$TEST_DIR" != "." ]; then
    TEST_FLAG="-test-directory=$TEST_DIR"
fi
# Resolve the input plugin tree to an absolute path up front: tofu resolves a
# relative -plugin-dir against the -chdir directory, and $PWD here is the
# action's exec root. The script never cd's, so capturing $PWD inline is safe.
PLUGIN_DIR="$PWD/$PLUGIN_TREE_ROOT"
SCRATCH=$(mktemp -d "${TMPDIR:-/tmp}/rules_tofu-validate-XXXXXX")
trap 'rm -rf "$SCRATCH"' EXIT
# A library with no files and no deps declares nothing under its work tree,
# so there is none to copy: an empty module, valid like any other.
if [ -d "$WORK_TREE_ROOT" ]; then
    cp -RL "$WORK_TREE_ROOT"/. "$SCRATCH"/
fi
# Zero-providers case: no files are declared under the plugin root, so its
# input directory is absent from the sandbox. Fall back to an empty dir so
# init runs offline and installs nothing.
if [ ! -d "$PLUGIN_DIR" ]; then
    PLUGIN_DIR="$SCRATCH/.rules_tofu-empty-plugins"
    mkdir -p "$PLUGIN_DIR"
fi
CWD="$SCRATCH/$PACKAGE_DIR"
if [ -e "$CWD/.terraform.lock.hcl" ]; then
    echo "rules_tofu: refusing to validate: .terraform.lock.hcl present in work tree under $PACKAGE_DIR. " \\
         "Lock files are managed implicitly via Bazel's provider pinning; remove it from srcs/data." 1>&2
    exit 1
fi
# The root module's directory is in the work tree only if some file is at or
# below it. One that isn't there is an empty module, which this creates
# (issue #119), before anything is copied into it.
mkdir -p "$CWD/.terraform/modules"
# In a directory without configuration files `tofu init` does nothing
# ("initialized in an empty directory"): it installs no provider, and leaves
# a lock as it finds it, which validate then holds against the empty plugin
# cache. An empty root module requires no provider, so it gets no lock.
# What tofu counts: files, and not those whose names start with a dot, which
# these patterns don't match either (tfconfig.ConfigFiles).
HAS_CONFIG=""
for f in "$CWD"/*.tf "$CWD"/*.tf.json "$CWD"/*.tofu "$CWD"/*.tofu.json; do
    if [ -f "$f" ]; then
        HAS_CONFIG=1
        break
    fi
done
if [ -n "$LOCK" ] && [ -n "$HAS_CONFIG" ]; then
    cp "$LOCK" "$CWD/.terraform.lock.hcl"
fi
# Every module call is already in the work tree; the manifest says where, and
# -get=false makes init check the calls against it rather than fetch.
cp "$MODULES" "$CWD/.terraform/modules/modules.json"
# Redirect init stdout to suppress "Installing provider" progress spam.
# Stderr is kept so error messages (e.g. "Failed to query available provider
# packages") and the "Incomplete lock file" warning remain visible.
# There is no flag to skip lockfile generation — the lockfile is written into
# $SCRATCH and discarded with it. (Not -lockfile=readonly: that also fails on
# lock entries this configuration doesn't use.)
if ! "$TOFU" -chdir="$CWD" init -backend=false -input=false -get=false -plugin-dir="$PLUGIN_DIR" ${TEST_FLAG:+"$TEST_FLAG"} >/dev/null; then
    if [ -n "$LOCK" ]; then
        echo 'rules_tofu: if the provider versions in required_providers changed, run' \
             '`bazel run @tf_providers//:pin` to update the lock file.' 1>&2
    fi
    if grep -q '"Key":"[^"]' "$MODULES"; then
        echo 'rules_tofu: modules are never fetched, so ignore any advice to run `tofu init`:' \
             "each module call's source and version must match the tf_library in deps that provides it" \
             '(its module_source and module_version).' 1>&2
    fi
    exit 1
fi
if ! "$TOFU" -chdir="$CWD" validate ${TEST_FLAG:+"$TEST_FLAG"}; then
    # The one way an empty root module is not like any other: tofu sets up
    # no providers for it, whatever the modules its test runs load require.
    if [ -z "$HAS_CONFIG" ] && [ -n "$TEST_DIR" ]; then
        echo "rules_tofu: the root module, ${PACKAGE_DIR:-the workspace root}, has no configuration file," \\
             'and `tofu init` installs no providers for one. If a provider is reported missing, give the' \\
             'root module a file whose `required_providers` names it.' 1>&2
    fi
    exit 1
fi
touch "$STAMP"
""",
        arguments = [
            tofu.binary.path,
            work_tree_root,
            package_dir,
            plugin_tree_root,
            stamp.path,
            modules_manifest.path,
        ] + [lock.path if lock else ""] + ([test_dir or "."] if test_dir != None else []),
        env = {"TF_IN_AUTOMATION": "1"},
        mnemonic = "TofuValidate",
        progress_message = "TofuValidate %{label}",
    )
    return stamp
