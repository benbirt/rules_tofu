"""The tf_test macro and its underlying rule."""

load("//toolchain:toolchain.bzl", "TOOLCHAIN_TYPE")
load(":init_action.bzl", _tf_init_validate = "tf_init_validate")
load(":providers.bzl", "TfDeployInfo", "TfLibraryInfo", "TfProviderInfo")
load(
    ":work_tree.bzl",
    "MODULES_BIN",
    "TEST_SRC_EXTS",
    _check_no_auto_tfvars = "check_no_auto_tfvars",
    _check_no_config_in_data = "check_no_config_in_data",
    _files_entries = "files_entries",
    _materialize = "materialize",
    _materialize_plugin_tree = "materialize_plugin_tree",
    _modules_manifest = "modules_manifest",
    _own_dirs = "own_dirs",
    _plugin_tree_root = "plugin_tree_root",
    _provider_lock = "provider_lock",
    _strict_deps = "strict_deps",
    _runfiles_path = "runfiles_path",
    _work_tree_path = "work_tree_path",
    _work_tree_root = "work_tree_root",
)

_RUNNER_BIN_LABEL = "//tf/private/cmd/runner:runner"

def _under_test(target):
    """What the test needs of the module under test, a tf_library or tf_deploy.

    Everything comes from the target's provider, `package_dir` included: an
    `alias` as `module` passes the provider through but reports its own label,
    so `target.label` would name the alias's package, not the module's.
    """
    if TfDeployInfo in target:
        info = target[TfDeployInfo]
        return struct(
            files = info.transitive_files,
            providers = info.providers,
            manifest = struct(
                module = None,
                modules = info.modules,
                dirs = info.dirs,
                modules_manifest = info.modules_manifest,
            ),
            stamps = info.stamps,
            tfvars_content = info.tfvars_content,
            var_files = info.var_files,
            package_dir = info.package_dir,
        )
    info = target[TfLibraryInfo]
    return struct(
        files = info.transitive_files,
        providers = info.providers,
        manifest = info,
        stamps = [info.validate_stamp],
        tfvars_content = None,
        var_files = [],
        package_dir = info.package_dir,
    )

def _join(a, b):
    if not a:
        return b
    if not b:
        return a
    return a + "/" + b

def _test_dir(ctx, root):
    """The test files' directory relative to `root`, "" for `root` itself.

    `tofu test` reads the test files of the root module's directory and of
    one test directory, so every file in `srcs` must be in one directory, at
    or below `root`.
    """
    prefix = root + "/" if root else ""
    dirs = {}
    for f in ctx.files.srcs:
        p = _work_tree_path(f)
        if not p.startswith(prefix):
            fail(("`{}`: test file `{}` is not in the directory of `{}` (`{}`) or below it. " +
                  "`tofu test` reads test files from the module's own directory or one subdirectory of it.").format(
                ctx.label,
                p,
                ctx.attr.module.label,
                root or "the workspace root",
            ))
        dirs[p[len(prefix):].rpartition("/")[0]] = True
    if len(dirs) > 1:
        fail(("`{}`: test files must be in one directory, the module's own or one below it, but are in {}. " +
              "`tofu test` reads one test directory; use one `tf_test` per directory.").format(
            ctx.label,
            ", ".join(["`{}`".format(_join(root, d) or ".") for d in sorted(dirs.keys())]),
        ))
    return dirs.keys()[0]

def _tf_test_impl(ctx):
    under_test = _under_test(ctx.attr.module)
    root = under_test.package_dir
    test_dir = _test_dir(ctx, root)

    # Assemble the work tree from the module under test, including a
    # deploy's var_files and generated tfvars, together with the test files,
    # their data and var_files and the helper libraries' files.
    _check_no_auto_tfvars(ctx, ctx.files.data)
    _check_no_config_in_data(ctx, ctx.files.data, srcs_take_configuration = False)
    direct = _files_entries(ctx.files.srcs + ctx.files.data + ctx.files.var_files)
    files = depset(
        direct = direct,
        transitive = [under_test.files] + [d[TfLibraryInfo].transitive_files for d in ctx.attr.deps],
    )
    work_tree_outputs = _materialize(
        ctx,
        files.to_list(),
        tfvars_content = under_test.tfvars_content,
        tfvars_dir = root,
    )

    direct_providers = [p[TfProviderInfo] for p in ctx.attr.providers]
    providers_depset = depset(
        direct = direct_providers,
        transitive = [under_test.providers] + [d[TfLibraryInfo].providers for d in ctx.attr.deps],
    )
    plugin_outputs = _materialize_plugin_tree(ctx, providers_depset)
    lock = _provider_lock(ctx, providers_depset)

    # Include `module` alongside `deps` so `run` blocks in `srcs` can call its
    # `module_source` with `//<subdir>` and load non-root directories from its
    # `srcs` and `data`. Files in `module` were already checked when `module`
    # was built and are not in `ctx.files.srcs` or `ctx.files.data`.
    infos = [under_test.manifest] + [d[TfLibraryInfo] for d in ctx.attr.deps]
    manifest = _modules_manifest(
        ctx,
        work_tree_outputs,
        infos,
        _strict_deps(direct_providers, infos),
        ctx.files.srcs,
        ctx.files.data,
        root = root,
        test_dir = test_dir,
    )

    work_tree_files = depset(direct = work_tree_outputs + plugin_outputs)
    validate_stamp = _tf_init_validate(
        ctx,
        work_tree_files = work_tree_files,
        work_tree_root = _work_tree_root(ctx),
        package_dir = root,
        plugin_tree_root = _plugin_tree_root(ctx),
        modules_manifest = manifest,
        required_stamps = under_test.stamps + [d[TfLibraryInfo].validate_stamp for d in ctx.attr.deps],
        lock = lock,
        test_dir = test_dir,
    )

    tofu = ctx.toolchains[TOOLCHAIN_TYPE].tofu
    runner_bin = ctx.attr._runner_bin[DefaultInfo].files_to_run.executable
    workspace_name = ctx.workspace_name or "_main"

    # The outputs are declared in this package: the work tree root in
    # runfiles is `<package in runfiles>/<name>.work` and the plugin tree its
    # sibling, the package's runfiles directory being the manifest's.
    package_rel = _runfiles_path(workspace_name, manifest).rpartition("/")[0]
    work_tree_root = "{}/{}.work".format(package_rel, ctx.label.name)
    plugin_tree_root = "{}/{}.plugins".format(package_rel, ctx.label.name)

    # A deploy's var_files come first and the test's own follow, as later
    # -var-file flags win. Paths are work-tree-relative because the runner
    # copies the tree into $TEST_TMPDIR.
    var_file_flags = "".join([
        '    --var-file="{}" \\\n'.format(_work_tree_path(f))
        for f in under_test.var_files + ctx.files.var_files
    ])
    test_dir_flag = ""
    if test_dir:
        test_dir_flag = '    --test-dir="{}" \\\n'.format(test_dir)
    lock_flag = ""
    if lock:
        lock_flag = '    --lock-file="$R/{}" \\\n'.format(_runfiles_path(workspace_name, lock))

    out = ctx.actions.declare_file(ctx.label.name + ".sh")
    ctx.actions.write(
        output = out,
        is_executable = True,
        content = """\
#!/usr/bin/env bash
# Generated by rules_tofu's tf_test rule. Do not edit.
set -euo pipefail
R="${{TEST_SRCDIR:-${{0}}.runfiles}}"
exec "$R/{runner}" \\
    --tofu="$R/{tofu}" \\
    --work-tree="$R/{work_tree}" \\
    --package-dir="{pkg}" \\
    --plugin-dir="$R/{plugin_tree}" \\
    --modules-manifest="$R/{manifest}" \\
    --command=test \\
{test_dir_flag}{lock_flag}{var_file_flags}    -- "$@"
""".format(
            runner = _runfiles_path(workspace_name, runner_bin),
            tofu = _runfiles_path(workspace_name, tofu.binary),
            work_tree = work_tree_root,
            pkg = root,
            plugin_tree = plugin_tree_root,
            manifest = _runfiles_path(workspace_name, manifest),
            test_dir_flag = test_dir_flag,
            lock_flag = lock_flag,
            var_file_flags = var_file_flags,
        ),
    )

    # Include validate_stamp in runfiles so bazel test runs tofu test only
    # after the test work tree, module and deps validate; in DefaultInfo.files
    # alone, validation runs in parallel with the test.
    runfiles = ctx.runfiles(
        files = [tofu.binary, manifest, validate_stamp] + ([lock] if lock else []) + work_tree_outputs + plugin_outputs,
    ).merge(ctx.attr._runner_bin[DefaultInfo].default_runfiles)

    return [DefaultInfo(
        executable = out,
        files = depset(direct = [out, validate_stamp]),
        runfiles = runfiles,
    )]

_tf_test = rule(
    implementation = _tf_test_impl,
    test = True,
    attrs = {
        "srcs": attr.label_list(
            allow_files = TEST_SRC_EXTS,
            allow_empty = False,
            mandatory = True,
            doc = "`.tftest.hcl`, `.tofutest.hcl`, `.tftest.json` and `.tofutest.json` files, " +
                  "all in one directory at or below `module`'s package directory. `.tofutest.hcl` " +
                  "and `.tofutest.json` shadow `.tftest.hcl` and `.tftest.json` of the same name.",
        ),
        "module": attr.label(
            mandatory = True,
            providers = [[TfLibraryInfo], [TfDeployInfo]],
            doc = "The `tf_library` or `tf_deploy` under test. Its package directory is the root " +
                  "module `tofu test` runs against; a deploy's `vars` and `var_files` apply.",
        ),
        "deps": attr.label_list(
            providers = [TfLibraryInfo],
            doc = "Helper `tf_library` targets loaded by `run { module { source = ... } }` blocks, " +
                  "by a path relative to `module`'s package directory or by `module_source`.",
        ),
        "data": attr.label_list(
            allow_files = True,
            doc = "Files placed in the work tree for `file()` and `templatefile()`. " +
                  "Configuration files are rejected; list them in the `srcs` of a " +
                  "`tf_library` in `deps`. " +
                  "`terraform.tfvars`, `terraform.tfvars.json`, `*.auto.tfvars` and " +
                  "`*.auto.tfvars.json` are rejected; use `var_files`.",
        ),
        "providers": attr.label_list(
            providers = [TfProviderInfo],
            doc = "Providers needed beyond those of `module` and `deps`, as " +
                  "`@tf_providers//<namespace>/<name>`. `mock_provider` still requires the " +
                  "provider package for its schema.",
        ),
        "var_files": attr.label_list(
            allow_files = [".tfvars.json", ".tfvars"],
            doc = "`.tfvars` and `.tfvars.json` files passed via `-var-file` after a deploy's own " +
                  "`vars` and `var_files`; `variables` blocks in the test files take precedence.",
        ),
        "_modules_bin": attr.label(
            default = MODULES_BIN,
            executable = True,
            cfg = "exec",
        ),
        "_runner_bin": attr.label(
            default = _RUNNER_BIN_LABEL,
            executable = True,
            cfg = "exec",
        ),
    },
    toolchains = [TOOLCHAIN_TYPE],
    doc = "Underlying rule for `tf_test`. Use the macro.",
)

def tf_test(name, srcs, module, deps = None, data = None, providers = None, var_files = None, allow_network = False, **kwargs):
    """Runs `tofu test` over a module's test files as a Bazel test.

    Test files are materialised at their workspace-relative paths alongside
    `module`; directories other than `module`'s root are passed via
    `-test-directory`. Helper modules loaded by `run` blocks are recorded in
    `<name>.modules.json` and installed at `.terraform/modules/modules.json` so
    `tofu` fetches nothing.

    `tofu test`'s JSON event log is written to Bazel's undeclared test outputs
    as `tofu-test.json`. If a `run` block's destroy fails, `tofu test` writes
    `errored_test.tfstate` and exits 0; `tf_test` fails the test and copies
    that file into undeclared outputs. Pass extra CLI flags via `--test_arg`.
    See [Tests](https://github.com/benbirt/rules_tofu#tests).

    `:<name>` is the only generated target. `bazel build` validates `module`
    and `srcs` after `module` and `deps` pass their checks. `bazel test` waits
    for validation, then copies the work tree into `$TEST_TMPDIR` and runs
    `tofu init -backend=false` and `tofu test` offline against the vendored
    providers.

    Args:
      name: Target name.
      srcs: `.tftest.hcl`, `.tofutest.hcl`, `.tftest.json` and
          `.tofutest.json` files, all in one directory at or below `module`'s
          package directory, such as `module`'s own directory, `tests/` or
          `tests/unit`. `.tofutest.hcl` and `.tofutest.json` shadow
          `.tftest.hcl` and `.tftest.json` of the same name.
      module: The `tf_library` or `tf_deploy` under test; may be an `alias`.
      deps: Helper `tf_library` targets loaded by
          `run { module { source = ... } }` blocks, reached by a path relative
          to `module`'s package directory or by `module_source`. Include any
          library from `module`'s `deps` that a `run` block in `srcs` loads
          directly. See
          [Strict dependencies](https://github.com/benbirt/rules_tofu#strict-dependencies).
      data: Files placed in the work tree at their workspace-relative paths for
          `file()` and `templatefile()`. Configuration files are rejected; list
          them in the `srcs` of a `tf_library` in `deps`. `terraform.tfvars`,
          `terraform.tfvars.json`, `*.auto.tfvars` and `*.auto.tfvars.json` are
          rejected; use `var_files`.
      providers: `@tf_providers//<namespace>/<name>` targets needed beyond
          those of `module` and `deps`. `mock_provider` still needs the
          provider package for its schema. If `module`'s root directory has no
          configuration files, `tofu init` installs no providers; add a file
          declaring `required_providers` there when helper modules require
          providers.
      var_files: `.tfvars` or `.tfvars.json` files passed via `-var-file` after
          a tested `tf_deploy`'s `vars` and `var_files`. Entries here may
          override keys from the deploy, and `variables` blocks in test files
          take precedence over both.
      allow_network: Allow tests to access the network. Defaults to `False`,
          adding Bazel's `block-network` tag so accidental cloud calls fail in
          the sandbox while plans and mocked applies run offline. When `True`,
          the target carries `external` and `requires-network` and is never
          cached.
      **kwargs: Attributes forwarded to the underlying test rule, such as
          `tags`, `size` and `timeout`.
    """
    tags = list(kwargs.pop("tags", []))
    if allow_network:
        tags = tags + ["external", "requires-network"]
    else:
        tags = tags + ["block-network"]

    _tf_test(
        name = name,
        srcs = srcs,
        module = module,
        deps = deps or [],
        data = data or [],
        providers = providers or [],
        var_files = var_files or [],
        tags = tags,
        **kwargs
    )
