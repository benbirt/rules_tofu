"""Work tree, plugin tree and module manifest helpers for `tf_library`, `tf_deploy` and `tf_test`.

A work tree is the directory under `bazel-bin/<pkg>/<name>.work/` into which a
rule symlinks its configuration, data, test and `var_files` inputs at their
`work_tree_path`. Main-repository files keep their workspace-relative path and
external files go under `external/<repo>/...`. The vendored provider packages
live in a sibling directory, `bazel-bin/<pkg>/<name>.plugins/`, so
`tofu init -plugin-dir=<plugin_tree>` reads them in place without linking
provider packages into the work tree and run directory.

`materialize` builds the work tree, `materialize_plugin_tree` builds the
sibling plugin tree and `modules_manifest` writes `<name>.modules.json`.
"""

load("//toolchain:toolchain.bzl", "TOOLCHAIN_TYPE")

# Only structural OpenTofu inputs are allowed in a rule's srcs. Variable values
# come from vars on tf_deploy and var_files on tf_deploy or tf_test. Allowing
# .tfvars or .tfvars.json here would let a reusable library supply variable
# values.
#
# `.tofu` and `.tofu.json` are OpenTofu-only configuration files: tofu reads
# `foo.tofu` and `foo.tofu.json` in place of `foo.tf` and `foo.tf.json` beside
# them.
ALLOWED_SRC_EXTS = [".tf", ".tf.json", ".tofu", ".tofu.json", ".tftpl", ".hcl"]

# The files `tofu test` reads. They go in the srcs of a `tf_test`, never of a
# `tf_library` or `tf_deploy`: tofu reads them from the root module's
# directory, so a test in a library's srcs would be shipped to, and loaded
# by, every dependant.
TEST_SRC_EXTS = [".tftest.hcl", ".tofutest.hcl", ".tftest.json", ".tofutest.json"]

# What tofu reads as a module's configuration (tfconfig.IsConfigFile).
_CONFIG_EXTS = (".tf", ".tf.json", ".tofu", ".tofu.json")

def check_srcs_and_data(ctx, srcs_are_tests = False):
    """Fail if the rule's `srcs` or `data` hold files of the wrong kind.

    `tf_library`, `tf_deploy` and `tf_test` each call this once so the `data`
    checks apply uniformly and non-test rules reject test files in `srcs`.

    Args:
      ctx: rule ctx with `srcs` and `data` attributes.
      srcs_are_tests: True for a `tf_test`, whose `srcs` are test files.
    """
    if not srcs_are_tests:
        _check_no_test_files(ctx, ctx.files.srcs)
    _check_no_auto_tfvars(ctx, ctx.files.data)
    _check_no_config_in_data(ctx, ctx.files.data, srcs_take_configuration = not srcs_are_tests)

def _check_no_test_files(ctx, files):
    """Fail if any of `files`, a library's or deploy's srcs, is a test file.

    Args:
      ctx: rule ctx, for the label in the error.
      files: list[File], the rule's `srcs`.
    """
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

def _check_no_auto_tfvars(ctx, files):
    """Fail if any of `files`, a rule's data, is a variables file `tofu` loads by itself.

    OpenTofu automatically reads `terraform.tfvars[.json]` and
    `*.auto.tfvars[.json]` from the root module's directory, and `tofu test`
    reads them from the test directory too. One in `data` would set variables
    alongside `vars` and `var_files` without being read by the duplicate-key
    check.

    `var_files` are not checked here: `tofu` loads them via `-var-file` in the
    order the rule chooses, and a deploy's are checked against `vars` and each
    other.

    The names are refused in any directory, not only a root module's: the
    listing rule cannot know which directory will be a root, and a library's
    package is the root of its own validation and of any `tf_test` of it.
    """
    for f in files:
        if _loaded_by_itself(f.basename):
            fail(("`{}`: `{}` in `data` is a variables file that OpenTofu auto-loads without " +
                  "duplicate-key checks. Set the values with `vars`, or list the file in the " +
                  "`var_files` of a `tf_deploy` or `tf_test` (README, \"Restrictions\").").format(
                ctx.label,
                f.short_path,
            ))

def _check_no_config_in_data(ctx, files, srcs_take_configuration = True):
    """Fail if any of `files`, a rule's data, is a configuration file.

    `data` holds what `file()` and `templatefile()` read. A configuration
    file there would be code that nothing checks: only `srcs` are checked
    against the rule's `providers` and `deps`. `tofu` passes over a file
    whose name starts with a dot, so such a file is data whatever its
    extension.

    Args:
      ctx: rule ctx.
      files: list[File], the rule's `data`.
      srcs_take_configuration: whether the rule's own `srcs` could list the
          file; False for a `tf_test`, whose `srcs` are test files.
    """
    for f in files:
        if f.basename.endswith(_CONFIG_EXTS) and not f.basename.startswith("."):
            where = "List it in `srcs`, or in the `srcs` of a `tf_library` in `deps`"
            if not srcs_take_configuration:
                where = "List it in the `srcs` of a `tf_library` in `deps`"
            fail("`{}`: `{}` in `data` is a configuration file. {} (README, \"Restrictions\").".format(
                ctx.label,
                f.short_path,
                where,
            ))

def runfiles_path(workspace_name, f):
    """Return the path at which `f` appears in a runfiles tree.

    Convention:
      - Main-repo files: `<workspace_name>/<short_path>`.
      - External-repo files: `<short_path>` with the leading `../` stripped.

    Args:
      workspace_name: string, the main repository's runfiles directory name.
      f: File.

    Returns:
      string: path relative to the runfiles root.
    """
    sp = f.short_path
    if sp.startswith("../"):
        return sp[3:]
    return workspace_name + "/" + sp

def shell_quote(s):
    """Return `s` single-quoted as one shell word for generated scripts."""
    return "'" + s.replace("'", "'\\''") + "'"

def var_flags(ctx, vars):
    """Return `--var=<name>=<value>` runner flags for `vars`, sorted by name.

    Fails analysis if a variable name is empty or contains `=`. `tofu` splits a
    `-var` argument at the first `=`, so `a=b` would assign to `a`.

    Args:
      ctx: rule ctx, for the label in the error.
      vars: dict[string, string], a `tf_deploy`'s `vars`.

    Returns:
      list[string]: one `--var=<name>=<value>` flag per entry.
    """
    flags = []
    for name, value in sorted(vars.items()):
        if not name or "=" in name:
            fail("`{}`: variable names in `vars` must not be empty or contain `=` (got `{}`).".format(
                ctx.label,
                name,
            ))
        flags.append("--var={}={}".format(name, value))
    return flags

def check_has_configuration(ctx, entries):
    """Fail a deploy whose root module holds no configuration file.

    `tofu` validates an empty directory and then refuses to plan or apply it,
    so such a deploy would build and never run. The usual cause is taking
    `deps` for what gets deployed: they are files in the work tree, which
    only a `module` block in the root module's own files reaches. Only a
    deploy is checked: a library may just bundle non-root files, and a test's
    `run` blocks may load other modules.

    Args:
      ctx: rule ctx.
      entries: list[struct(path, file)] work-tree entries (`files_entries`).
    """
    root = package_dir(ctx.label)
    for entry in entries:
        directory, _, name = entry.path.rpartition("/")

        # tofu passes over a file whose name starts with a dot.
        if directory == root and name.endswith(_CONFIG_EXTS) and not name.startswith("."):
            return
    fail(("`{label}` has no configuration file in its root module's directory, {dir}: there is nothing " +
          "to deploy, and tofu refuses to plan it. `deps`, and files in subdirectories, are only reached " +
          "by `module` blocks in files of that directory: add `srcs` there that call them.").format(
        label = display_label(ctx),
        dir = "`{}/`".format(root) if root else "the workspace root",
    ))

def work_tree_path(f):
    """The path at which `f` appears in a work tree.

    Main-repository files keep their workspace-relative path. Files from
    other repositories go under `external/<canonical repo name>/`, which no
    main-repository package can occupy because Bazel reserves `//external`.
    HCL reaches them by `module_source`, never by a relative path, since the
    canonical names are Bazel's to choose.

    Args:
      f: File.

    Returns:
      string: path relative to the work tree root.
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

def materialize(ctx, entries):
    """Materialise a work tree under `<pkg>/<name>.work/`.

    For each `struct(path, file)` in `entries`, declares `<name>.work/<path>`
    and symlinks it to `file`.

    Fails if any entry in any directory is named `.terraform.lock.hcl` or if
    two distinct files share a work-tree path.

    Args:
      ctx: rule ctx.
      entries: list[struct(path, file)] of work-tree paths (`work_tree_path`) and Files.

    Returns:
      list[File]: the declared work-tree symlinks.
    """
    work_prefix = ctx.label.name + ".work"
    outputs = []
    seen = {}
    for entry in entries:
        path = entry.path
        if path.rpartition("/")[2] == ".terraform.lock.hcl":
            fail(("`{}`: `{}` is an OpenTofu provider lock file, which `srcs` and `data` cannot " +
                  "include because `rules_tofu` manages the provider lock itself. Remove or exclude " +
                  "it from `srcs` and `data`, and pass the workspace's lock to " +
                  "`tf_providers.from_lock` in `MODULE.bazel` (README, \"Restrictions\").").format(
                ctx.label,
                entry.file.short_path,
            ))
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

    return outputs

def materialize_plugin_tree(ctx, providers_depset):
    """Symlink provider packages into this rule's sibling plugin tree.

    Declares outputs under `<name>.plugins/` beside the work tree once per
    provider `address`, using OpenTofu's canonical plugin-dir layout
    `<host>/<namespace>/<name>/<version>/<os>_<arch>/`: a symlink to every
    file from the unpacked zip so the directory matches the lock's `h1:` hash
    and `tofu init` locates the binary.

    Through public rules only `@tf_providers` creates `TfProviderInfo`, one
    target per address, so duplicate addresses are deduplicated without
    checking `version`. Each `TfProviderInfo` carries one platform's package,
    selected for the execution platform of the target listing it, while one
    plugin tree serves a single `tofu` binary. Fails if any two entries
    disagree on `platform`, even for different addresses.

    Args:
      ctx: rule ctx.
      providers_depset: depset[TfProviderInfo].

    Returns:
      list[File]: declared symlink outputs, empty when no providers are in scope.
    """
    plugin_prefix = ctx.label.name + ".plugins"

    seen = {}
    first = None
    outputs = []
    for prov in providers_depset.to_list():
        # Check against `first` rather than `seen[prov.address]`: each target
        # resolves `cfg = "exec"` for its own `providers`, so a target and its
        # `deps` can contribute distinct providers for different platforms.
        if first == None:
            first = prov
        elif first.platform != prov.platform:
            fail(
                ("`{label}` receives providers for two execution platforms: `{a}` and `{b}` " +
                 "(`{addr_a}` and `{addr_b}`). Configure `{label}` and its `deps` to resolve the " +
                 "same execution platform (for example with matching `exec_compatible_with`).").format(
                    label = ctx.label,
                    a = first.platform,
                    b = prov.platform,
                    addr_a = first.address,
                    addr_b = prov.address,
                ),
            )
        if prov.address in seen:
            continue
        seen[prov.address] = True

        parts = prov.address.split("/")
        if len(parts) != 3:
            fail("invalid provider address `{}` (expected `<host>/<ns>/<name>`)".format(prov.address))
        platform_dir = "{prefix}/{host}/{ns}/{name}/{version}/{plat}".format(
            prefix = plugin_prefix,
            host = parts[0],
            ns = parts[1],
            name = parts[2],
            version = prov.version,
            plat = prov.platform,
        )
        for entry in prov.files:
            out = ctx.actions.declare_file(platform_dir + "/" + entry.path)
            ctx.actions.symlink(output = out, target_file = entry.file)
            outputs.append(out)

    return outputs

def provider_lock(ctx, providers_depset):
    """The `.terraform.lock.hcl` pinning the providers in scope, or None.

    Only providers from `tf_providers.from_lock` carry one, and a workspace
    has a single lock, so every provider that carries one carries the same.

    Args:
      ctx: rule ctx, for the label in the error.
      providers_depset: depset[TfProviderInfo].

    Returns:
      File or None: the lock file, or None when no provider carries one.
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

    Sibling of `work_tree_root(ctx)`; the vendored provider packages
    declared by `materialize_plugin_tree` live under it.
    """
    return "{bin}/{pkg}/{name}.plugins".format(
        bin = ctx.bin_dir.path,
        pkg = package_dir(ctx.label),
        name = ctx.label.name,
    )

MODULES_BIN = "//tf/private/cmd/modules:modules"

def library_module(label, module_source, module_version, dirs):
    """`TfLibraryInfo.module` for a library with these attributes, or None.

    Args:
      label: Label of the `tf_library`.
      module_source: string, the library's `module_source` attribute.
      module_version: string, the library's `module_version` attribute.
      dirs: list[string], work-tree directories of the library's files
          (`own_dirs`).

    Returns:
      struct(label, source, version, dir, dirs) or None: None when
      `module_source` is empty.
    """
    if not module_source:
        if module_version:
            fail("`{}` sets module_version without module_source".format(label))
        return None
    return struct(
        label = _display_label(label),
        source = module_source,
        version = module_version,
        dir = package_dir(label),
        # A tuple, since a depset carries the struct.
        dirs = tuple(dirs),
    )

def own_dirs(files):
    """Sorted unique work-tree directories containing `files`."""
    return sorted({work_tree_path(f).rpartition("/")[0]: None for f in files}.keys())

def strict_deps(providers, infos):
    """Collect the direct providers, modules and directories a target may use (issue #135).

    Args:
      providers: list[TfProviderInfo] from the target's `providers`.
      infos: direct dependency structs as passed to `modules_manifest`.

    Returns:
      struct(providers, modules, dirs): string lists of direct provider
      addresses, direct `module_source` labels, and work-tree directories of
      direct dependencies' `srcs` and `data`.
    """
    return struct(
        providers = [p.address for p in providers],
        modules = [info.module.label for info in infos if info.module],
        dirs = [d for info in infos for d in info.dirs],
    )

def modules_manifest(ctx, work_tree_outputs, infos, strict, srcs, data, root = None, test_dir = None, own = None):
    """Emit the action writing this rule's `.terraform/modules/modules.json`.

    See //tf/private/cmd/modules: every module call from the root module down
    is recorded, local ones by their directory in the work tree and others by
    the dep whose `module_source` they name, so `tofu init -get=false` finds
    them all installed. The runner copies the manifest into place for
    validation, tests and deploys. The action also fails when `srcs` use a
    provider or module absent from `strict`, other than `own`, or call a
    directory that holds no files from `srcs`, `data` or `strict`.

    Args:
      ctx: rule ctx. Must list `TOOLCHAIN_TYPE` in `toolchains` (the action
          runs the toolchain's `tofu`) and declare `_modules_bin`
          (cfg = "exec", `MODULES_BIN`) on the rule.
      work_tree_outputs: list[File], the work tree's files (`materialize`).
      infos: the rule's deps, each a `TfLibraryInfo` or a struct with its
          `module`, `modules`, `dirs` and `modules_manifest` fields. The
          module under test of a `tf_test` comes first. Each entry's
          `modules_manifest` is an unused input of this action so upstream
          manifest checks run first (issue #143).
      strict: direct dependencies allowed by strict-deps checks (`strict_deps`).
      srcs: list[File], the rule's `srcs`.
      data: list[File], the rule's `data`, which holds no configuration.
      root: the root module's work-tree directory; the rule's own package by
          default. A `tf_test`'s is the module under test's.
      test_dir: for a `tf_test`, the test files' directory relative to `root`,
          or "" for `root` itself; the module each `run` block loads is
          recorded too. None for a rule with no test files.
      own: for a `tf_library` with `module_source`, its own
          `TfLibraryInfo.module`: its files may call that address with
          `//<subdir>`, as its callers do. Listed as a module the calls can
          resolve to, never as a direct dep, which must be used.

    Returns:
      File: the manifest.
    """
    direct = {label: True for label in strict.modules}
    modules = depset(
        direct = [own] if own else [],
        transitive = [info.modules for info in infos],
    ).to_list()
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
                    dirs = m.dirs,
                    direct = m.label in direct,
                    callable = m.label in direct or (own != None and m.label == own.label),
                )
                for m in modules
            ],
            own_srcs = [work_tree_path(f) for f in srcs],
            own_data = [work_tree_path(f) for f in data],
            dep_dirs = sorted({d: None for d in strict.dirs}.keys()),
        )),
    )

    tofu = ctx.toolchains[TOOLCHAIN_TYPE].tofu
    manifest = ctx.actions.declare_file(ctx.label.name + ".modules.json")
    args = ctx.actions.args()
    args.add("--tofu", tofu.binary)
    args.add("--work-tree", work_tree_root(ctx))
    args.add("--root", root if root != None else package_dir(ctx.label))
    args.add("--modules", listing)
    args.add("--label", display_label(ctx))
    args.add_all(sorted({p: None for p in strict.providers}.keys()), before_each = "--direct-provider")
    if test_dir != None:
        args.add("--test-dir", test_dir or ".")
    args.add("--out", manifest)
    ctx.actions.run(
        executable = ctx.attr._modules_bin[DefaultInfo].files_to_run,
        inputs = [listing, tofu.binary] + work_tree_outputs + [info.modules_manifest for info in infos],
        outputs = [manifest],
        arguments = [args],
        mnemonic = "TofuModules",
        progress_message = "TofuModules " + display_label(ctx),
    )
    return manifest

def display_label(ctx):
    """The target's public label (`@tf_modules//<path>` when generated)."""
    return getattr(ctx.attr, "tf_modules_label", "") or _display_label(ctx.label)

def _display_label(label):
    """`label` as users write it: `//pkg:name` in the main repository."""
    if label.workspace_root:
        return str(label)
    return "//{}:{}".format(label.package, label.name)
