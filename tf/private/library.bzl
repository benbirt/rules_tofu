"""`tf_library` rule: a reusable bundle of OpenTofu/Terraform
configuration files, plus transitive `tf_library` deps. Not
directly runnable; bind variable values and produce runnable
`.plan`/`.apply` targets with `tf_deploy`.

Building a library runs `tofu init -backend=false && tofu validate`
against its transitive files as part of the build action — the target
will not build if validation fails. There is no separate `.validate`
sub-target.
"""

load("//toolchain:toolchain.bzl", "TOOLCHAIN_TYPE")
load(":fmt.bzl", _tf_fmt = "tf_fmt", _tf_fmt_check = "tf_fmt_check_test")
load(":init_action.bzl", _tf_init_validate = "tf_init_validate")
load(":providers.bzl", "TfLibraryInfo", "TfProviderInfo")
load(
    ":work_tree.bzl",
    "ALLOWED_SRC_EXTS",
    "MODULES_BIN",
    _files_entries = "files_entries",
    _library_module = "library_module",
    _materialize = "materialize",
    _materialize_plugin_tree = "materialize_plugin_tree",
    _modules_manifest = "modules_manifest",
    _package_dir = "package_dir",
    _plugin_tree_root = "plugin_tree_root",
    _provider_lock = "provider_lock",
    _work_tree_root = "work_tree_root",
)

def _tf_library_impl(ctx):
    direct = _files_entries(ctx.files.srcs + ctx.files.data)
    transitive = [
        d[TfLibraryInfo].transitive_files
        for d in ctx.attr.deps
    ]
    files = depset(direct = direct, transitive = transitive)

    direct_providers = [p[TfProviderInfo] for p in ctx.attr.providers]
    transitive_providers = [
        d[TfLibraryInfo].providers
        for d in ctx.attr.deps
    ]
    providers_depset = depset(direct = direct_providers, transitive = transitive_providers)

    module = _library_module(ctx.label, ctx.attr.module_source, ctx.attr.module_version)
    modules = depset(
        direct = [module] if module else [],
        transitive = [d[TfLibraryInfo].modules for d in ctx.attr.deps],
    )

    # Materialize a work tree (no tfvars — libraries carry no var values) and
    # symlink the exec-platform binary for each provider into the plugin
    # tree. Then run init+validate; the stamp lives in DefaultInfo.files so
    # `bazel build :foo` fails when validation fails.
    work_tree_outputs = _materialize(ctx, files.to_list(), tfvars_content = None)
    plugin_outputs = _materialize_plugin_tree(ctx, providers_depset)
    work_tree_files = depset(direct = work_tree_outputs + plugin_outputs)

    validate_stamp = _tf_init_validate(
        ctx,
        work_tree_files = work_tree_files,
        work_tree_root = _work_tree_root(ctx),
        package_dir = _package_dir(ctx.label),
        plugin_tree_root = _plugin_tree_root(ctx),
        modules_manifest = _modules_manifest(ctx, work_tree_outputs, ctx.attr.deps),
        lock = _provider_lock(ctx, providers_depset),
    )

    return [
        DefaultInfo(files = depset(direct = [validate_stamp])),
        TfLibraryInfo(
            transitive_files = files,
            providers = providers_depset,
            module = module,
            modules = modules,
        ),
    ]

_tf_library = rule(
    implementation = _tf_library_impl,
    attrs = {
        "srcs": attr.label_list(
            allow_files = ALLOWED_SRC_EXTS,
            doc = "Source files belonging to this library (.tf, .tf.json, .tftpl, .hcl).",
        ),
        "deps": attr.label_list(
            providers = [TfLibraryInfo],
            doc = "Other `tf_library` targets whose files this library composes with.",
        ),
        "data": attr.label_list(
            allow_files = True,
            doc = "Arbitrary files to include alongside the Terraform sources in the work tree. " +
                  "Use to expose files for `file()` calls in Terraform configs.",
        ),
        "providers": attr.label_list(
            providers = [TfProviderInfo],
            doc = "Vendored OpenTofu/Terraform providers this library references. " +
                  "Declare each provider once in MODULE.bazel via the `tf_providers` " +
                  "extension and pass `@tf_providers//<namespace>/<name>` here. " +
                  "Propagates transitively to " +
                  "any `tf_deploy` that pulls this library in.",
        ),
        "module_source": attr.string(
            doc = "The address by which HCL calls this library as a module, e.g. " +
                  "`\"git::https://github.com/acme/infra.git//vpc?ref=v1.2.0\"`: the `source` that " +
                  "users outside Bazel would write. A `module` block whose `source` is this " +
                  "address (or this address plus `//<subdir>`) uses this library's files, " +
                  "when the library is in the caller's `deps`. Needed to call a library " +
                  "from another Bazel repository, which a relative path can't reach.",
        ),
        "module_version": attr.string(
            doc = "The version of this module, required when `module_source` is a module " +
                  "registry address. A caller's `version` constraint must accept it.",
        ),
        "_modules_bin": attr.label(
            default = MODULES_BIN,
            executable = True,
            cfg = "exec",
        ),
    },
    toolchains = [TOOLCHAIN_TYPE],
    doc = """Bundles a set of OpenTofu/Terraform configuration files for reuse.

A `tf_library` carries no variable values and is not directly
runnable. Use `tf_deploy` to bind variable values and produce
`:foo.plan` / `:foo.apply` runnable sub-targets.

Building a library runs `tofu init -backend=false && tofu validate` against
its transitive files, so configuration errors fail the build.

At runtime, every file in `srcs` is materialized at its
workspace-relative path, so:
  - Same-package files reference each other bare.
  - Cross-package deps reference each other with
    `source = "../other_lib"`.

Note: Terraform parses any `source` not starting with `./` or `../` as
a registry address, so to reach a top-level workspace directory from a
deeply nested module, traverse with `../../...`.

A library from another Bazel repository is called by its `module_source`
instead, the address that users outside Bazel would write. rules_tofu writes
every module call into `.terraform/modules/modules.json`, as `tofu init`
does when it installs modules, and runs `tofu init -get=false`, so no module
is ever fetched. A call whose `source` no dep provides fails the build, as
does a dep with `module_source` that no call uses.
""",
)

def tf_library(name, srcs = None, deps = None, data = None, providers = None, module_source = None, module_version = None, fmt_test = True, **kwargs):
    """A reusable bundle of OpenTofu/Terraform configuration files.

    Always generates:
      - `:<name>`     — the library target. Building it runs
                        `tofu init -backend=false && tofu validate` against
                        the library's transitive files.
      - `:<name>.fmt` — `bazel run` to reformat .tf files in-place.

    When `fmt_test=True` (the default) and `srcs` is non-empty:
      - `:<name>.fmt_check`  — `bazel test` that fails if files are not formatted.

    Args:
      name: target name.
      srcs: source .tf/.tf.json/.tftpl/.hcl files.
      deps: other `tf_library` targets to compose with.
      data: arbitrary files to include in the work tree for `file()` calls.
      providers: providers (`@tf_providers//<namespace>/<name>`, from the
          `tf_providers` extension in MODULE.bazel) that this library
          references in `required_providers`. Propagates transitively to any
          `tf_deploy` consuming this library.
      module_source: the address by which HCL calls this library as a module,
          as users outside Bazel would write it in `source`. Callers with this
          library in `deps` use its files for a `module` block with that
          `source` (or that plus `//<subdir>`).
      module_version: this module's version. Required when `module_source` is
          a module registry address.
      fmt_test: whether to emit a `:<name>.fmt_check` test target. Only emitted
          when True (the default) and `srcs` is non-empty.
      **kwargs: forwarded to the underlying rule (visibility, tags, testonly).
    """
    common_kwargs = {}
    for forwarded in ("visibility", "tags", "testonly"):
        if forwarded in kwargs:
            common_kwargs[forwarded] = kwargs.pop(forwarded)

    _tf_library(
        name = name,
        srcs = srcs or [],
        deps = deps or [],
        data = data or [],
        providers = providers or [],
        module_source = module_source or "",
        module_version = module_version or "",
        **dict(common_kwargs, **kwargs)
    )

    _tf_fmt(name = name + ".fmt", **common_kwargs)
    if fmt_test and srcs:
        _tf_fmt_check(
            name = name + ".fmt_check",
            srcs = srcs,
            **common_kwargs
        )
