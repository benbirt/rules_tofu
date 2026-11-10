"""Work tree materialization shared between `tf_library` and
`tf_deploy`.

A "work tree" is the directory under `bazel-bin/<pkg>/<name>.work/` into
which a rule symlinks every transitive `.tf`/data file at its
workspace-relative path, plus (for deploys) a generated
`rules_tofu.auto.tfvars.json`. The vendored provider plugin tree lives in a
sibling directory, `bazel-bin/<pkg>/<name>.plugins/`, so `tofu init
-plugin-dir=<plugin_tree>` reads the providers in place without the work
tree ever carrying the (large) provider binaries.

`materialize(...)` handles the .tf/data half (with optional tfvars).
`materialize_plugin_tree(...)` handles the per-provider symlinks.
"""

load("//toolchain:toolchain.bzl", "TOOLCHAIN_TYPE")
load(":providers.bzl", "TfLibraryInfo")

# Only structural Terraform inputs are allowed in a rule's srcs. Variable
# values come from `tf_deploy(vars = {...})`; allowing `.tfvars[.json]`
# here would create ambiguous precedence with the deploy-emitted
# `rules_tofu.auto.tfvars.json` and lets a reusable module declare values
# it has no business owning.
#
# `.tofu` and `.tofu.json` are OpenTofu-only configuration files: tofu reads
# `foo.tofu` in place of a `foo.tf` beside it.
ALLOWED_SRC_EXTS = [".tf", ".tf.json", ".tofu", ".tofu.json", ".tftpl", ".hcl"]

# The files `tofu test` reads. They go in the srcs of a `tf_test`, never of a
# `tf_library` or `tf_deploy`: tofu reads them from the root module's
# directory, so a test in a library's srcs would be shipped to, and loaded
# by, every dependant.
TEST_SRC_EXTS = [".tftest.hcl", ".tofutest.hcl", ".tftest.json", ".tofutest.json"]

def check_no_test_files(ctx, files):
    """Fail if any of `files`, a library's or deploy's srcs, is a test file."""
    for f in files:
        for ext in TEST_SRC_EXTS:
            if f.basename.endswith(ext):
                fail(("`{}`: `{}` is an OpenTofu test file; list it in the srcs of a `tf_test` " +
                      "with this target as its `module` instead (README, \"Tests\").").format(
                    ctx.label,
                    f.short_path,
                ))

def _loaded_by_itself(basename):
    return (
        basename in ["terraform.tfvars", "terraform.tfvars.json"] or
        basename.endswith(".auto.tfvars") or
        basename.endswith(".auto.tfvars.json")
    )

def check_no_auto_tfvars(ctx, files):
    """Fail if any of `files`, a rule's data, is a variables file tofu loads by itself.

    OpenTofu reads `terraform.tfvars[.json]` and `*.auto.tfvars[.json]` from
    the root module's directory (and `tofu test` from the test directory)
    without being told to. One that arrived as `data` would set variables
    beside `vars` and `var_files`, over them if its name sorts after
    `rules_tofu.auto.tfvars.json`, and the duplicate-key check never reads
    it. `var_files` are not checked here: those tofu is told to load, in an
    order the rule chooses, and a deploy's are checked against `vars` and
    each other.

    The names are refused in any directory, not only a root module's: which
    directory is a root is not the listing rule's to know (a library's
    package is the root of its own validation and of any `tf_test` of it).
    """
    for f in files:
        if _loaded_by_itself(f.basename):
            fail(("`{}`: `{}` in `data` is a variables file that OpenTofu loads by itself, where " +
                  "nothing checks it against `vars` and `var_files`. Set the values with `vars`, or " +
                  "list the file in the `var_files` of a `tf_deploy` or `tf_test` (README, " +
                  "\"Restrictions\").").format(
                ctx.label,
                f.short_path,
            ))

def runfiles_path(workspace_name, f):
    """Return the path at which `f` appears in a runfiles tree.

    Convention:
      - Main-repo files: `<workspace_name>/<short_path>`.
      - External-repo files: `<short_path>` with the leading `../` stripped.
    """
    sp = f.short_path
    if sp.startswith("../"):
        return sp[3:]
    return workspace_name + "/" + sp

def work_tree_path(f):
    """The path at which `f` appears in a work tree.

    Main-repository files keep their workspace-relative path. Files from
    other repositories go under `external/<canonical repo name>/`, which no
    main-repository package can occupy (Bazel reserves `//external`). HCL
    reaches them by `module_source`, never by a relative path, since the
    canonical names are Bazel's to choose.
    """
    sp = f.short_path
    if sp.startswith("../"):
        return "external/" + sp[3:]
    return sp

def package_dir(label):
    """The directory of `label`'s package in a work tree (see `work_tree_path`)."""
    return _join(label.workspace_root, label.package)

def _join(a, b):
    if not a:
        return b
    if not b:
        return a
    return a + "/" + b

def files_entries(files):
    """`struct(path, file)` for each of `files`, at its `work_tree_path`."""
    return [struct(path = work_tree_path(f), file = f) for f in files]

def materialize(ctx, entries, tfvars_content = None, tfvars_dir = None):
    """Materialize a work tree under `<pkg>/<name>.work/`.

    For each `struct(path, file)` in `entries`, declares `<name>.work/<path>`
    and symlinks it to `file`. If `tfvars_content` is non-None, also writes
    `<name>.work/<package>/rules_tofu.auto.tfvars.json` with that content.

    Args:
      ctx: rule ctx.
      entries: list[struct(path, file)] — work-tree paths (`work_tree_path`) + Files.
      tfvars_content: optional string. Pass None for library work trees
          (which carry no variable values).
      tfvars_dir: the work-tree directory to write the tfvars file in; the
          rule's own package by default. A `tf_test` writes the deploy under
          test's in that deploy's directory.

    Returns:
      list[File]: the declared work-tree outputs (symlinks plus, when
      `tfvars_content` is set, the generated tfvars file).
    """
    work_prefix = ctx.label.name + ".work"
    outputs = []
    seen = {}
    for entry in entries:
        path = entry.path
        if path in seen:
            other = seen[path]
            if other != entry.file:
                fail(
                    "File collision at workspace path `{}` between `{}` and `{}`. ".format(
                        path,
                        other.path,
                        entry.file.path,
                    ) + "Two libraries are contributing different content at the same path.",
                )
            continue
        seen[path] = entry.file
        out = ctx.actions.declare_file(work_prefix + "/" + path)
        ctx.actions.symlink(output = out, target_file = entry.file)
        outputs.append(out)

    if tfvars_content != None:
        tfvars_path = work_prefix + "/" + _join(
            tfvars_dir if tfvars_dir != None else package_dir(ctx.label),
            "rules_tofu.auto.tfvars.json",
        )
        if tfvars_path[len(work_prefix) + 1:] in seen:
            fail(
                "`{}` collides with the generated rules_tofu.auto.tfvars.json. ".format(
                    seen[tfvars_path[len(work_prefix) + 1:]].path,
                ) + "Rename that file: the name is reserved for the values in `vars`.",
            )
        tfvars_file = ctx.actions.declare_file(tfvars_path)
        ctx.actions.write(output = tfvars_file, content = tfvars_content)
        outputs.append(tfvars_file)

    return outputs

def materialize_plugin_tree(ctx, providers_depset):
    """Symlink provider binaries into this rule's sibling plugin tree.

    Declares outputs under `<name>.plugins/` (a sibling of the work tree),
    for each unique `(address, version)`, using Terraform's canonical
    plugin-dir layout `<host>/<namespace>/<name>/<version>/<os>_<arch>/`: a
    symlink to the binary and to each other file from the provider's zip, so
    the directory hashes to the lock's `h1:` value.

    Args:
      ctx: rule ctx. Must list `TOOLCHAIN_TYPE` in `toolchains` so
          `tofu.platform_key` is available.
      providers_depset: depset[TfProviderInfo].

    Returns:
      list[File]: declared symlink outputs (possibly empty).

    Fails if two providers share an address but differ in version, or if a
    declared provider has no binary for the exec platform.
    """
    tofu = ctx.toolchains[TOOLCHAIN_TYPE].tofu
    platform_key = tofu.platform_key
    plugin_prefix = ctx.label.name + ".plugins"

    seen_versions = {}
    outputs = []
    for prov in providers_depset.to_list():
        prior = seen_versions.get(prov.address)
        if prior != None:
            if prior != prov.version:
                fail(
                    ("`{label}` has conflicting versions for provider `{addr}`: " +
                     "`{a}` vs `{b}`. Pick one in MODULE.bazel.").format(
                        label = ctx.label,
                        addr = prov.address,
                        a = prior,
                        b = prov.version,
                    ),
                )
            continue
        seen_versions[prov.address] = prov.version

        binary = prov.binaries.get(platform_key)
        if binary == None:
            fail(
                ("`{label}` requires provider `{addr}@{ver}` for exec platform " +
                 "`{plat}`, but it was vendored without one: add `{plat}` to its " +
                 "`sha256` map in MODULE.bazel, or check that the registry publishes it.").format(
                    label = ctx.label,
                    addr = prov.address,
                    ver = prov.version,
                    plat = platform_key,
                ),
            )

        parts = prov.address.split("/")
        if len(parts) != 3:
            fail("invalid provider address `{}` (expected `<host>/<ns>/<name>`)".format(prov.address))
        platform_dir = "{prefix}/{host}/{ns}/{name}/{version}/{plat}".format(
            prefix = plugin_prefix,
            host = parts[0],
            ns = parts[1],
            name = parts[2],
            version = prov.version,
            plat = platform_key,
        )
        files = [struct(path = binary.basename, file = binary)] + prov.package_files.get(platform_key, [])
        for entry in files:
            out = ctx.actions.declare_file(platform_dir + "/" + entry.path)
            ctx.actions.symlink(output = out, target_file = entry.file)
            outputs.append(out)

    return outputs

def provider_lock(ctx, providers_depset):
    """The `.terraform.lock.hcl` pinning the providers in scope, or None.

    Only providers from `tf_providers.from_lock` carry one, and a workspace
    has a single lock, so every provider that carries one carries the same.
    """
    locks = {}
    for prov in providers_depset.to_list():
        if prov.lock != None:
            locks[prov.lock] = prov.address
    if len(locks) > 1:
        fail("`{}` has providers pinned by different lock files: {}".format(
            ctx.label,
            ", ".join(["{} ({})".format(f.short_path, addr) for f, addr in locks.items()]),
        ))
    return locks.keys()[0] if locks else None

def work_tree_root(ctx):
    """Exec-root-relative path to this rule's work tree root."""
    return "{bin}/{pkg}/{name}.work".format(
        bin = ctx.bin_dir.path,
        pkg = package_dir(ctx.label),
        name = ctx.label.name,
    )

def plugin_tree_root(ctx):
    """Exec-root-relative path to this rule's plugin tree root.

    Sibling of `work_tree_root(ctx)`; the vendored provider binaries
    declared by `materialize_plugin_tree(...)` live under it.
    """
    return "{bin}/{pkg}/{name}.plugins".format(
        bin = ctx.bin_dir.path,
        pkg = package_dir(ctx.label),
        name = ctx.label.name,
    )

MODULES_BIN = "//tf/private/cmd/modules:modules"

def library_module(label, module_source, module_version):
    """`TfLibraryInfo.module` for a library with these attributes, or None."""
    if not module_source:
        if module_version:
            fail("`{}` sets module_version without module_source".format(label))
        return None
    return struct(
        label = _display_label(label),
        source = module_source,
        version = module_version,
        dir = package_dir(label),
    )

def modules_manifest(ctx, work_tree_outputs, infos, providers_depset, root = None, test_dir = None):
    """Emit the action writing this rule's `.terraform/modules/modules.json`.

    See //tf/private/cmd/modules: every module call from the root module down
    is recorded, local ones by their directory in the work tree and others by
    the dep whose `module_source` they name, so `tofu init -get=false` finds
    them all installed. The validate action and the runner copy the manifest
    into place. The action also fails when a module requires a provider that
    `providers_depset` lacks, naming the label to add: a submodule called by
    `//<subdir>` outside `modules/` carries none of its own (issue #76).

    Args:
      ctx: rule ctx. Must declare `_modules_bin` (cfg = "exec") on the rule.
      work_tree_outputs: list[File], the work tree's files (`materialize`).
      infos: the rule's deps, each a `TfLibraryInfo` or a struct with its
          `module`, `modules` and `packages` fields. The module under test of
          a `tf_test` comes first.
      providers_depset: depset[TfProviderInfo], the providers in the rule's
          plugin tree.
      root: the root module's work-tree directory; the rule's own package by
          default. A `tf_test`'s is the module under test's.
      test_dir: for a `tf_test`, the test files' directory relative to `root`
          ("" for `root` itself); the module each `run` block loads is
          recorded too. None for a rule with no test files.

    Returns:
      File: the manifest.
    """
    direct = {}
    for info in infos:
        if info.module:
            direct[info.module.label] = True
    modules = depset(transitive = [info.modules for info in infos]).to_list()
    listing = ctx.actions.declare_file(ctx.label.name + ".modules_deps.json")
    ctx.actions.write(
        output = listing,
        content = json.encode(struct(
            modules = [
                struct(
                    label = m.label,
                    source = m.source,
                    version = m.version,
                    dir = m.dir,
                    direct = m.label in direct,
                )
                for m in modules
            ],
            # Which library a directory belongs to, for the unused-dep check.
            packages = depset(transitive = [info.packages for info in infos]).to_list(),
        )),
    )

    manifest = ctx.actions.declare_file(ctx.label.name + ".modules.json")
    args = ctx.actions.args()
    args.add("--work-tree", work_tree_root(ctx))
    args.add("--root", root if root != None else package_dir(ctx.label))
    args.add("--modules", listing)
    args.add("--label", _display_label(ctx.label))
    args.add_all(
        sorted({p.address: None for p in providers_depset.to_list()}.keys()),
        before_each = "--provider",
    )
    if test_dir != None:
        args.add("--test-dir", test_dir or ".")
    args.add("--out", manifest)
    ctx.actions.run(
        executable = ctx.attr._modules_bin[DefaultInfo].files_to_run,
        inputs = [listing] + work_tree_outputs,
        outputs = [manifest],
        arguments = [args],
        mnemonic = "TofuModules",
        progress_message = "TofuModules %{label}",
    )
    return manifest

def _display_label(label):
    """`label` as users write it: `//pkg:name` in the main repository."""
    if label.workspace_root:
        return str(label)
    return "//{}:{}".format(label.package, label.name)
