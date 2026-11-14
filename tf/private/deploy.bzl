"""`tf_deploy` rule + macro.

The macro always emits:
  - `:<name>`         — the `_tf_deploy` data target. Its outputs are
                        the materialized working tree (a symlink-mirror of
                        every transitive .tf input at its workspace-relative
                        path, plus a generated `rules_tofu.auto.tfvars.json`),
                        a validate stamp produced by running
                        `tofu init -backend=false && tofu validate` at build
                        time, and (when `var_files` is non-empty) a stamp
                        produced by a duplicate-variable-key check across
                        `vars` and every `var_files` entry. So
                        `bazel build :<name>` exercises both checks.
  - `:<name>.plan`    — runnable: `bazel run :<name>.plan`
  - `:<name>.apply`   — runnable: `bazel run :<name>.apply`
  - `:<name>.destroy` — runnable: `bazel run :<name>.destroy`

When `srcs` is non-empty:
  - `:<name>.fmt`     — runnable: `bazel run :<name>.fmt`

When additionally `fmt_test=True` (the default):
  - `:<name>.fmt_check`  — test: `bazel test :<name>.fmt_check`

Materialization happens at analysis time via `ctx.actions.symlink` (one
action per file). At run time the runner mirrors the work tree into the
deploy's run directory, `<name>.rules_tofu-state/work/`, and invokes
`tofu init && tofu <plan|apply|destroy>` there. Nothing is mktemp'd;
nothing is symlinked from bash.
"""

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
            doc = "Optional deploy-local config files (e.g. provider/backend setup). " +
                  "Test files (.tftest.hcl, .tofutest.hcl) go in a `tf_test`.",
        ),
        "deps": attr.label_list(
            providers = [TfLibraryInfo],
            doc = "`tf_library` targets whose files join the deploy's work tree. Those in the " +
                  "deploy's own package are part of its root module; others are reached by " +
                  "`module` blocks in the root module's files.",
        ),
        "vars": attr.string_dict(
            doc = "Variable values bound to this deploy, each a string. Rendered to " +
                  "rules_tofu.auto.tfvars.json, where OpenTofu converts a string to a variable's " +
                  "declared `number` or `bool` type; a list, map or object value goes in `var_files`.",
        ),
        "var_files": attr.label_list(
            allow_files = [".tfvars.json", ".tfvars"],
            doc = "Variable-value files passed to every tofu invocation via -var-file. " +
                  "Accepts any Label producing a .tfvars.json or .tfvars file (e.g. a genrule output). " +
                  "Keys in var_files must not overlap with keys in vars or other var_files entries.",
        ),
        "data": attr.label_list(
            allow_files = True,
            doc = "Arbitrary files to include in the work tree. " +
                  "Use to expose files for `file()` calls in Terraform configs. " +
                  "Variables files that OpenTofu loads by itself (`terraform.tfvars[.json]`, " +
                  "`*.auto.tfvars[.json]`) are rejected: set the values with `vars`, or list " +
                  "the file in `var_files`.",
        ),
        "providers": attr.label_list(
            providers = [TfProviderInfo],
            doc = "Vendored OpenTofu/Terraform providers this deploy declares directly. " +
                  "Unioned with providers contributed transitively by `deps`. " +
                  "Each entry is a `@tf_providers//<namespace>/<name>` target from the " +
                  "`tf_providers` extension in MODULE.bazel.",
        ),
        "allow_ephemeral_state": attr.bool(
            default = False,
            doc = "Permit `apply`/`destroy` to run against local, disposable state. " +
                  "With no `backend`/`cloud` block, or with a `backend \"local\"` whose " +
                  "`path` and `workspace_dir` aren't both literal absolute paths outside " +
                  "Bazel's output tree, state lands under " +
                  "`bazel-bin/<pkg>/<name>.rules_tofu-state/` — machine-local, keyed by " +
                  "build configuration, and wiped by `bazel clean` — so the runner refuses " +
                  "those commands by default. Set True only where losing state is harmless: " +
                  "demos, throwaway sandboxes, or bootstrapping the bucket that will hold real " +
                  "state. `plan` is never gated.",
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
    doc = "Underlying data-carrier for `tf_deploy`. Use the macro.",
)

def tf_deploy(name, srcs = None, deps = None, vars = None, var_files = None, data = None, providers = None, allow_ephemeral_state = False, fmt_test = True, **kwargs):
    """A root OpenTofu/Terraform configuration, with targets to plan, apply and destroy it.

    Always generates:
      - `:<name>`         — data target. Building it materializes the work tree
                            and runs `tofu init -backend=false && tofu validate`
                            against it, so `bazel build :<name>` validates.
      - `:<name>.plan`    — `bazel run` to produce a plan.
      - `:<name>.apply`   — `bazel run` to apply.
      - `:<name>.destroy` — `bazel run` to destroy all managed resources.

    The three are ordinary executables: besides `bazel run`, the built
    `bazel-bin/<pkg>/<name>.<command>.sh` can be run directly, from any
    directory. tofu runs in the deploy's run directory,
    `bazel-bin/<pkg>/<name>.rules_tofu-state/work/<pkg>`, which they share.

    When `srcs` is non-empty:
      - `:<name>.fmt`     — `bazel run` to reformat the .tf and .tofu files in
                            `srcs` in place.

    When additionally `fmt_test=True` (the default):
      - `:<name>.fmt_check` — `bazel test` that fails if those files are not formatted.

    Args:
      name: target name.
      srcs: optional deploy-local .tf/.tf.json/.tofu/.tofu.json/.tftpl/.hcl files (e.g. provider/backend config).
      deps: `tf_library` targets whose files join the deploy's work tree.
          Those in the deploy's own package are part of its root module;
          others are reached by `module` blocks in the root module's files.
          The deploy's package must hold a configuration file, from `srcs`
          or from a library in that package: `deps` in other packages alone
          deploy nothing, and fail to build.
      vars: dict of variable name -> string value, rendered to
          rules_tofu.auto.tfvars.json. OpenTofu converts a string to a
          variable's declared `number` or `bool` type; a list, map or object
          value goes in `var_files`.
      var_files: `.tfvars.json` or `.tfvars` files passed to tofu with `-var-file`.
          Any label works, e.g. a genrule output. Keys must not overlap with
          `vars` or other `var_files` entries; a duplicate key fails
          `bazel build`.
      data: arbitrary files to include in the work tree, enabling `file()` calls
          in Terraform configs. Variables files that OpenTofu loads by itself
          (`terraform.tfvars[.json]`, `*.auto.tfvars[.json]`) are rejected:
          set the values with `vars`, or list the file in `var_files`.
      providers: providers (`@tf_providers//<namespace>/<name>`, from the
          `tf_providers` extension in MODULE.bazel) declared at the deploy
          level, in addition to those its `deps` bring in. `tofu init` runs
          offline against them.
      allow_ephemeral_state: allow `:<name>.apply` and `:<name>.destroy` to run
          against local state under `bazel-bin`. State lives there for a
          deploy with no `backend "..." {}` or `cloud {}` block (at
          `bazel-bin/<pkg>/<name>.rules_tofu-state/terraform.tfstate`), and for
          one with a `backend "local"` block unless its `path` and
          `workspace_dir` are both literal absolute paths outside Bazel's
          output tree: tofu resolves anything else against the deploy's run
          directory, `bazel-bin/<pkg>/<name>.rules_tofu-state/work/<pkg>`.
          State there is machine-local, forks with the build configuration,
          and is deleted by `bazel clean` — losing it orphans whatever was
          applied. So by default the runner refuses both mutating commands for
          such a deploy; `plan` is never gated. Set True only where disposable
          state is the point: demos and examples, throwaway sandboxes, or
          bootstrapping the bucket that will hold real state (then move it
          with `tofu init -migrate-state` once the backend block is in place).
          Ignored when the deploy's backend keeps state elsewhere.
      fmt_test: whether to emit a `:<name>.fmt_check` test target. Only emitted
          when True (the default) and `srcs` is non-empty.
      **kwargs: forwarded to the underlying rule (visibility, tags, testonly).
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
