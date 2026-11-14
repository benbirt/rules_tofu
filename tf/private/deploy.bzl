"""The tf_deploy macro and its underlying rule."""

load("//toolchain:toolchain.bzl", "TOOLCHAIN_TYPE")
load(":fmt.bzl", _tf_fmt = "tf_fmt", _tf_fmt_check = "tf_fmt_check_test")
load(":init_action.bzl", _tf_init_validate = "tf_init_validate")
load(":providers.bzl", "TfDeployInfo", "TfLibraryInfo", "TfProviderInfo")
load(":runner.bzl", _tf_runner = "tf_runner")
load(":var_files_check.bzl", "DUPCHECK_BIN", _tf_check_var_files = "tf_check_var_files")
load(
    ":work_tree.bzl",
    "ALLOWED_SRC_EXTS",
    "MODULES_BIN",
    _check_has_configuration = "check_has_configuration",
    _check_no_auto_tfvars = "check_no_auto_tfvars",
    _check_no_test_files = "check_no_test_files",
    _files_entries = "files_entries",
    _materialize = "materialize",
    _materialize_plugin_tree = "materialize_plugin_tree",
    _modules_manifest = "modules_manifest",
    _package_dir = "package_dir",
    _plugin_tree_root = "plugin_tree_root",
    _provider_lock = "provider_lock",
    _work_tree_path = "work_tree_path",
    _work_tree_root = "work_tree_root",
)

def _tf_deploy_impl(ctx):
    _check_no_test_files(ctx, ctx.files.srcs)
    _check_no_auto_tfvars(ctx, ctx.files.data)
    direct = _files_entries(ctx.files.srcs + ctx.files.data)
    var_file_entries = _files_entries(ctx.files.var_files)
    transitive = [d[TfLibraryInfo].transitive_files for d in ctx.attr.deps]
    files = depset(direct = direct + var_file_entries, transitive = transitive)
    entries = files.to_list()
    _check_has_configuration(ctx, entries)

    tfvars_content = json.encode_indent(
        ctx.attr.vars,
        indent = "  ",
    )

    outputs = _materialize(ctx, entries, tfvars_content = tfvars_content)

    # Aggregate providers: direct + transitive via library deps.
    direct_providers = [p[TfProviderInfo] for p in ctx.attr.providers]
    transitive_provider_sets = [
        d[TfLibraryInfo].providers
        for d in ctx.attr.deps
    ]
    providers_depset = depset(direct = direct_providers, transitive = transitive_provider_sets)
    modules_manifest = _modules_manifest(
        ctx,
        outputs,
        [d[TfLibraryInfo] for d in ctx.attr.deps],
        providers_depset,
    )
    plugin_outputs = _materialize_plugin_tree(ctx, providers_depset)
    outputs = outputs + plugin_outputs
    lock = _provider_lock(ctx, providers_depset)

    work_tree_files = depset(direct = outputs)

    validate_stamp = _tf_init_validate(
        ctx,
        work_tree_files = work_tree_files,
        work_tree_root = _work_tree_root(ctx),
        package_dir = _package_dir(ctx.label),
        modules_manifest = modules_manifest,
        plugin_tree_root = _plugin_tree_root(ctx),
        lock = lock,
    )

    var_files_check_stamp = _tf_check_var_files(
        ctx,
        vars_keys = sorted(ctx.attr.vars.keys()),
        var_files = ctx.files.var_files,
    )

    default_files = [validate_stamp]
    if var_files_check_stamp != None:
        default_files.append(var_files_check_stamp)

    return [
        DefaultInfo(files = depset(direct = default_files, transitive = [work_tree_files])),
        TfDeployInfo(
            work_tree_files = work_tree_files,
            package_dir = _package_dir(ctx.label),
            modules_manifest = modules_manifest,
            var_file_relpaths = [_work_tree_path(f) for f in ctx.files.var_files],
            allow_ephemeral_state = ctx.attr.allow_ephemeral_state,
            lock = lock,
            transitive_files = files,
            providers = providers_depset,
            modules = depset(transitive = [d[TfLibraryInfo].modules for d in ctx.attr.deps]),
            packages = depset(
                direct = [_package_dir(ctx.label)],
                transitive = [d[TfLibraryInfo].packages for d in ctx.attr.deps],
            ),
            tfvars_content = tfvars_content,
            var_files = ctx.files.var_files,
        ),
    ]

tf_deploy_rule = rule(
    implementation = _tf_deploy_impl,
    attrs = {
        "srcs": attr.label_list(
            allow_files = ALLOWED_SRC_EXTS,
            doc = "Deploy-local `.tf`, `.tf.json`, `.tofu`, `.tofu.json`, `.tftpl` and `.hcl` " +
                  "files, typically `backend` and `provider` blocks. Test files belong in " +
                  "`tf_test.srcs`.",
        ),
        "deps": attr.label_list(
            providers = [TfLibraryInfo],
            doc = "`tf_library` targets included in the deploy's work tree. Same-package " +
                  "libraries join the root module; others are reached via `module` blocks.",
        ),
        "vars": attr.string_dict(
            doc = "String-valued variable bindings written to `rules_tofu.auto.tfvars.json`, " +
                  "reserving that filename in the deploy's package. OpenTofu converts strings " +
                  "for `number` and `bool` variables; pass lists, maps and objects via " +
                  "`var_files`.",
        ),
        "var_files": attr.label_list(
            allow_files = [".tfvars.json", ".tfvars"],
            doc = "`.tfvars` and `.tfvars.json` files passed via `-var-file`, including " +
                  "generated outputs. Keys must not overlap with `vars` or other `var_files`.",
        ),
        "data": attr.label_list(
            allow_files = True,
            doc = "Files placed in the work tree for `file()` and `templatefile()`. " +
                  "`terraform.tfvars`, `terraform.tfvars.json`, `*.auto.tfvars` and " +
                  "`*.auto.tfvars.json` are rejected; use `vars` or `var_files`.",
        ),
        "providers": attr.label_list(
            providers = [TfProviderInfo],
            doc = "Providers required directly by `srcs`, as `@tf_providers//<namespace>/<name>`, " +
                  "combined with providers propagated from `deps`.",
        ),
        "allow_ephemeral_state": attr.bool(
            default = False,
            doc = "Permit `apply` and `destroy` when state lives under `bazel-bin`: either no " +
                  "`backend` or `cloud` block, or a `backend \"local\"` without literal absolute " +
                  "`path` and `workspace_dir` outside `bazel-out`. `plan` is never gated. See " +
                  "[State and backends](https://github.com/benbirt/rules_tofu#state-and-backends).",
        ),
        "_dupcheck_bin": attr.label(
            default = DUPCHECK_BIN,
            executable = True,
            cfg = "exec",
        ),
        "_modules_bin": attr.label(
            default = MODULES_BIN,
            executable = True,
            cfg = "exec",
        ),
    },
    toolchains = [TOOLCHAIN_TYPE],
    doc = "Underlying rule for `tf_deploy`. Use the macro.",
)

def tf_deploy(name, srcs = None, deps = None, vars = None, var_files = None, data = None, providers = None, allow_ephemeral_state = False, fmt_test = True, **kwargs):
    """A root OpenTofu configuration, with targets to plan, apply and destroy it.

    The deploy's own package is its root module and must contain at least one
    `.tf`, `.tf.json`, `.tofu` or `.tofu.json` file from `srcs` or a
    `tf_library` in the same package; `deps` in other packages are reached only
    via `module` blocks.

    Generated targets:
      - `:<name>`: materialises the work tree, checks for duplicate variable
        keys across `vars` and `var_files`, and runs
        `tofu init -backend=false && tofu validate`.
      - `:<name>.plan`: `bazel run` target that runs `tofu plan`, saving the
        plan file to `<state-dir>/tfplan`.
      - `:<name>.apply`: `bazel run` target that runs a fresh `tofu apply`
        without reading `tfplan`. Prompts unless `-- -auto-approve` is passed.
      - `:<name>.destroy`: `bazel run` target that runs `tofu destroy`. Prompts
        unless `-- -auto-approve` is passed.
      - `:<name>.fmt`: `bazel run` target, emitted when `srcs` is non-empty,
        that reformats main-repository `.tf` and `.tofu` source files in `srcs`
        in place.
      - `:<name>.fmt_check`: `bazel test` target, emitted when `srcs` is
        non-empty and `fmt_test = True`, that checks formatting of `.tf` and
        `.tofu` files in `srcs`, including generated and external files.

    Each launcher, such as `bazel-bin/<pkg>/<name>.apply.sh`, can also be
    invoked directly in place from any working directory; do not copy or symlink
    the script elsewhere. All three share
    `bazel-bin/<pkg>/<name>.rules_tofu-state/work/<pkg>` without locking, so run
    one command at a time. See
    [How it works](https://github.com/benbirt/rules_tofu#how-it-works).

    Args:
      name: Target name.
      srcs: Deploy-local `.tf`, `.tf.json`, `.tofu`, `.tofu.json`, `.tftpl` and
          `.hcl` files, typically `backend` and `provider` blocks. Test files
          belong in `tf_test.srcs`.
      deps: `tf_library` targets included in the deploy's work tree. Those in
          the deploy's own package join its root module; others are reached via
          `module` blocks in the root module's files.
      vars: Dictionary of variable name to string value, written to
          `rules_tofu.auto.tfvars.json` in the deploy's package directory. That
          filename is reserved in the package. OpenTofu converts strings to
          `number` or `bool` where declared; pass lists, maps and objects via
          `var_files`.
      var_files: `.tfvars` or `.tfvars.json` files passed to `tofu` via
          `-var-file`, including outputs of other rules. Keys must not overlap
          with `vars` or other `var_files` entries; duplicates fail
          `bazel build`.
      data: Files placed in the work tree at their workspace-relative paths for
          `file()` and `templatefile()`. `terraform.tfvars`,
          `terraform.tfvars.json`, `*.auto.tfvars` and `*.auto.tfvars.json` are
          rejected; use `vars` or `var_files`.
      providers: `@tf_providers//<namespace>/<name>` targets required directly
          by `srcs`, combined with providers propagated from `deps`.
      allow_ephemeral_state: Permit `:<name>.apply` and `:<name>.destroy` when
          state lives under `bazel-bin`:
          - no `backend` or `cloud` block is declared; or
          - `backend "local"` does not set both `path` and `workspace_dir` to
            literal absolute paths outside `bazel-out`.
          Defaults to `False`. See
          [State and backends](https://github.com/benbirt/rules_tofu#state-and-backends).
      fmt_test: Emit `:<name>.fmt_check` when `True` and `srcs` is non-empty;
          defaults to `True`.
      **kwargs: Forwarded to the underlying rules. `visibility`, `tags` and
          `testonly` apply to all generated targets; other rule attributes
          apply to `:<name>`.
    """
    common_kwargs = {}
    for forwarded in ("visibility", "tags", "testonly"):
        if forwarded in kwargs:
            common_kwargs[forwarded] = kwargs.pop(forwarded)

    tf_deploy_rule(
        name = name,
        srcs = srcs or [],
        deps = deps or [],
        vars = vars or {},
        var_files = var_files or [],
        data = data or [],
        providers = providers or [],
        allow_ephemeral_state = allow_ephemeral_state,
        **dict(common_kwargs, **kwargs)
    )

    _tf_runner(
        name = name + ".plan",
        deploy = ":" + name,
        command = "plan",
        **common_kwargs
    )

    _tf_runner(
        name = name + ".apply",
        deploy = ":" + name,
        command = "apply",
        **common_kwargs
    )

    _tf_runner(
        name = name + ".destroy",
        deploy = ":" + name,
        command = "destroy",
        **common_kwargs
    )

    if srcs:
        _tf_fmt(name = name + ".fmt", srcs = srcs, **common_kwargs)
        if fmt_test:
            _tf_fmt_check(
                name = name + ".fmt_check",
                srcs = srcs,
                **common_kwargs
            )
