"""The tf_library macro and its underlying rule."""

load("//toolchain:toolchain.bzl", "TOOLCHAIN_TYPE")
load(":fmt.bzl", _tf_fmt = "tf_fmt", _tf_fmt_check = "tf_fmt_check_test")
load(":init_action.bzl", _tf_init_validate = "tf_init_validate")
load(":providers.bzl", "TfLibraryInfo", "TfProviderInfo")
load(
    ":work_tree.bzl",
    "ALLOWED_SRC_EXTS",
    "MODULES_BIN",
    _check_no_auto_tfvars = "check_no_auto_tfvars",
    _check_no_test_files = "check_no_test_files",
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
    _check_no_test_files(ctx, ctx.files.srcs)
    _check_no_auto_tfvars(ctx, ctx.files.data)
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

    # Materialise a work tree without tfvars, since libraries carry no
    # variable values, and symlink each provider's exec-platform package
    # into the plugin tree. Put the init+validate stamp in DefaultInfo.files
    # so `bazel build :foo` fails when validation fails.
    work_tree_outputs = _materialize(ctx, files.to_list(), tfvars_content = None)
    plugin_outputs = _materialize_plugin_tree(ctx, providers_depset)
    work_tree_files = depset(direct = work_tree_outputs + plugin_outputs)

    validate_stamp = _tf_init_validate(
        ctx,
        work_tree_files = work_tree_files,
        work_tree_root = _work_tree_root(ctx),
        package_dir = _package_dir(ctx.label),
        plugin_tree_root = _plugin_tree_root(ctx),
        modules_manifest = _modules_manifest(
            ctx,
            work_tree_outputs,
            [d[TfLibraryInfo] for d in ctx.attr.deps],
            providers_depset,
            own = module,
        ),
        lock = _provider_lock(ctx, providers_depset),
    )

    return [
        DefaultInfo(files = depset(direct = [validate_stamp])),
        TfLibraryInfo(
            transitive_files = files,
            providers = providers_depset,
            module = module,
            modules = modules,
            packages = depset(
                direct = [_package_dir(ctx.label)],
                transitive = [d[TfLibraryInfo].packages for d in ctx.attr.deps],
            ),
            package_dir = _package_dir(ctx.label),
        ),
    ]

_tf_library = rule(
    implementation = _tf_library_impl,
    attrs = {
        "srcs": attr.label_list(
            allow_files = ALLOWED_SRC_EXTS,
            doc = "Configuration and template files: `.tf`, `.tf.json`, `.tofu`, `.tofu.json`, " +
                  "`.tftpl` and `.hcl`. `.tofu` and `.tofu.json` shadow `.tf` and `.tf.json` " +
                  "of the same name. Test files belong in `tf_test.srcs`.",
        ),
        "deps": attr.label_list(
            providers = [TfLibraryInfo],
            doc = "`tf_library` targets whose files, providers and `module_source` mappings " +
                  "propagate through this library.",
        ),
        "data": attr.label_list(
            allow_files = True,
            doc = "Files placed in the work tree for `file()` and `templatefile()`. " +
                  "`terraform.tfvars`, `terraform.tfvars.json`, `*.auto.tfvars` and " +
                  "`*.auto.tfvars.json` are rejected; pass variable files via `var_files` on " +
                  "`tf_deploy` or `tf_test`.",
        ),
        "providers": attr.label_list(
            providers = [TfProviderInfo],
            doc = "Providers required by `srcs`, as `@tf_providers//<namespace>/<name>` targets " +
                  "from the `tf_providers` extension. Propagated transitively to dependants.",
        ),
        "module_source": attr.string(
            doc = "Address by which HCL `module` blocks call this library or `//<subdir>` " +
                  "within it when it is in the caller's `deps`, e.g. " +
                  "`\"git::https://github.com/acme/infra.git//vpc?ref=v1.2.0\"`. Required to " +
                  "call a library in another Bazel repository.",
        ),
        "module_version": attr.string(
            doc = "Version checked against callers' `version` constraints when `module_source` " +
                  "is a registry address. Required for registry addresses; setting it without " +
                  "`module_source` is an error.",
        ),
        "_modules_bin": attr.label(
            default = MODULES_BIN,
            executable = True,
            cfg = "exec",
        ),
    },
    toolchains = [TOOLCHAIN_TYPE],
    doc = "Underlying rule for `tf_library`. Use the macro.",
)

def tf_library(name, srcs = None, deps = None, data = None, providers = None, module_source = None, module_version = None, fmt_test = True, **kwargs):
    """A reusable bundle of OpenTofu and Terraform-compatible configuration files.

    A `tf_library` carries no variable values and has no `.plan`, `.apply` or
    `.destroy` targets; use `tf_deploy` for a root configuration.

    Files in the main repository are materialised at their workspace-relative
    paths, so same-package files see each other directly. Call modules in other
    workspace packages by relative paths starting with `./` or `../`, such as
    `../../infra/dns`.

    Module calls are resolved offline at build time:
      - Every `module` block's `source` must be a literal string. `override`
        and `*_override` files replace `source` on matching `module` blocks,
        as OpenTofu merges them.
      - Relative paths starting with `./` or `../` resolve within the caller's
        own Bazel repository in the work tree and cannot cross into another
        Bazel repository.
      - Other `source` strings match the `tf_library` in `deps` whose
        normalised `module_source` equals the address or leaves the shortest
        `//<subdir>` suffix, so a submodule's own library takes precedence over
        its parent module's library.
      - Registry addresses normalise to lower-case
        `<host>/<namespace>/<name>/<system>`, defaulting `<host>` to
        `registry.opentofu.org`.
      - Git shorthands normalise as OpenTofu parses them:
        `github.com/<owner>/<repo>[/<subdir>]` becomes
        `git::https://github.com/<owner>/<repo>.git[//<subdir>]`, and
        `git@<host>:<path>` becomes `git::ssh://git@<host>/<path>`. `?ref=`
        must match, while `?depth=` is ignored.
      - Archive URLs match by archive format and URL after stripping
        `?archive=`, so `.tgz` and `?archive=tar.gz` share `tar.gz`; any other
        query parameters must match.
      - A library with `module_source` also satisfies calls from its own files
        to that `module_source` with `//<subdir>`.

    `rules_tofu` writes `<name>.modules.json` at build time, copies it to
    `.terraform/modules/modules.json`, and runs `tofu init -get=false`.
    `bazel build` fails on:
      - A `module` block whose `source` is not a literal string or uses an
        unsupported source type.
      - A local `./` or `../` call to a directory outside the work tree, in
        another Bazel repository, or absent from `deps`.
      - A non-local `module` call that no `tf_library` in `deps` matches.
      - A direct `module_source` dependency unused by this target's own
        configuration. Calls inside other libraries' packages in `deps` do not
        count, whereas a `module_source` library in this target's own package
        or reached from it by `./` or `../` counts as used.
      - Two libraries in `deps` with the same normalised `module_source`.
      - A registry `module` call whose library has an empty `module_version` or
        fails the caller's `version` constraint.
      - A `module_source` call landing in a directory with no `.tf`,
        `.tf.json`, `.tofu` or `.tofu.json` files. Local `./` or `../` calls
        may target directories in the work tree that hold no configuration
        files.
      - A module call cycle.
      - A `required_providers` entry missing from `providers` or `deps`, or
        outside `registry.opentofu.org`. Built-in providers such as
        `terraform.io/builtin/terraform` are exempt.
    See [Modules](https://github.com/benbirt/rules_tofu#modules).

    Generated targets:
      - `:<name>`: materialises the work tree and runs
        `tofu init -backend=false && tofu validate`.
      - `:<name>.fmt`: `bazel run` target, emitted when `srcs` is non-empty,
        that reformats main-repository `.tf` and `.tofu` source files in `srcs`
        in place.
      - `:<name>.fmt_check`: `bazel test` target, emitted when `srcs` is
        non-empty and `fmt_test = True`, that checks formatting of `.tf` and
        `.tofu` files in `srcs`, including generated and external files.

    Args:
      name: Target name.
      srcs: `.tf`, `.tf.json`, `.tofu`, `.tofu.json`, `.tftpl` and `.hcl`
          files. `.tofu` and `.tofu.json` shadow `.tf` and `.tf.json` of the
          same name. OpenTofu ignores dot-prefixed filenames.
          `.terraform.lock.hcl` is rejected in `srcs` and `data`, and test
          files belong in `tf_test.srcs`.
      deps: `tf_library` targets whose files, providers and `module_source`
          mappings propagate through this library.
      data: Files placed in the work tree at their workspace-relative paths for
          `file()` and `templatefile()`. `terraform.tfvars`,
          `terraform.tfvars.json`, `*.auto.tfvars` and `*.auto.tfvars.json` are
          rejected; pass variable files via `var_files` on `tf_deploy` or
          `tf_test`.
      providers: `@tf_providers//<namespace>/<name>` targets required by
          `srcs`. Propagated transitively to dependants.
      module_source: Address by which HCL `module` blocks call this library or
          `//<subdir>` within it when it is in the caller's `deps`. Required to
          call a library from another Bazel repository.
      module_version: Version checked against callers' `version` constraints
          when `module_source` is a registry address. Required for registry
          addresses. Defaults to the version of the Bazel module that holds the
          library when `module_source` is set, or `""` otherwise; setting it
          without `module_source` is an error.
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

    # A registry module's version is normally its Bazel module's version, which
    # its publisher already bumps on every release.
    if module_version == None:
        module_version = (native.module_version() or "") if module_source else ""

    _tf_library(
        name = name,
        srcs = srcs or [],
        deps = deps or [],
        data = data or [],
        providers = providers or [],
        module_source = module_source or "",
        module_version = module_version,
        **dict(common_kwargs, **kwargs)
    )

    if srcs:
        _tf_fmt(name = name + ".fmt", srcs = srcs, **common_kwargs)
        if fmt_test:
            _tf_fmt_check(
                name = name + ".fmt_check",
                srcs = srcs,
                **common_kwargs
            )
