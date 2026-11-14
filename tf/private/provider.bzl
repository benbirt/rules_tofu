"""`tf_provider` rule.

A `tf_provider` target wraps the per-platform packages and optional lock file
of one OpenTofu provider version, such as `hashicorp/aws` at `5.70.0`.

Instantiated by the `tf_providers` module extension (see
`//tf/providers:extensions.bzl`); users do not call `tf_provider` directly.
Providers are pinned via `tf_providers.from_lock` or `tf_providers.provider` in
`MODULE.bazel` and referenced as `@tf_providers//<namespace>/<name>` in the
`providers` attribute of `tf_library`, `tf_deploy` or `tf_test`. Each generated
`tf_provider` target is visible only to its `@tf_providers` alias.
"""

load(":providers.bzl", "TfProviderInfo")

visibility(["public"])

def _tf_provider_impl(ctx):
    binaries = {}
    for tgt, platform_key in ctx.attr.binaries.items():
        files = tgt.files.to_list()
        if len(files) != 1:
            fail("tf_provider `{}` binary for {} must be a single file, got {}".format(
                ctx.label,
                platform_key,
                [f.path for f in files],
            ))
        binaries[platform_key] = files[0]

    # Each file sits at `plugins/<address>/<version>/<os>_<arch>/<path>` in
    # the provider's repo; the plugin tree wants `<path>`.
    package_files = {}
    for tgt, platform_key in ctx.attr.package_files.items():
        prefix = "plugins/{}/{}/{}/".format(ctx.attr.address, ctx.attr.version, platform_key)
        for f in tgt.files.to_list():
            if not f.owner.name.startswith(prefix):
                fail("tf_provider `{}`: {} is not under {}".format(ctx.label, f.path, prefix))
            package_files.setdefault(platform_key, []).append(
                struct(path = f.owner.name[len(prefix):], file = f),
            )

    return [
        DefaultInfo(files = depset(direct = binaries.values())),
        TfProviderInfo(
            address = ctx.attr.address,
            version = ctx.attr.version,
            binaries = binaries,
            package_files = package_files,
            lock = ctx.file.lock,
        ),
    ]

tf_provider = rule(
    implementation = _tf_provider_impl,
    attrs = {
        "address": attr.string(
            mandatory = True,
            doc = "Canonical provider address, e.g. \"registry.opentofu.org/hashicorp/aws\".",
        ),
        "version": attr.string(
            mandatory = True,
            doc = "Provider version, e.g. \"5.70.0\".",
        ),
        "binaries": attr.label_keyed_string_dict(
            mandatory = True,
            allow_files = True,
            doc = "Map of single-file provider binary label to `<os>_<arch>` platform key. " +
                  "Extracted at `plugins/<address>/<version>/<os>_<arch>/<filename>` in the " +
                  "provider repository; `materialize_plugin_tree` symlinks each binary under " +
                  "`<os>_<arch>/` by its `basename`.",
        ),
        "package_files": attr.label_keyed_string_dict(
            allow_files = True,
            doc = "Map of the remaining files from each platform's unpacked zip, such as " +
                  "`LICENSE` and `README.md`, to `<os>_<arch>` platform key. Each file's " +
                  "target name must start with `plugins/<address>/<version>/<os>_<arch>/`, " +
                  "which is stripped to give its path inside `<os>_<arch>/`.",
        ),
        "lock": attr.label(
            allow_single_file = True,
            doc = "Optional `.terraform.lock.hcl` from `tf_providers.from_lock`. Copied next " +
                  "to a root module's configuration before `tofu init` in validation, deploy " +
                  "runners and `tf_test`.",
        ),
    },
    provides = [TfProviderInfo],
    doc = "Wraps the per-platform packages and optional lock file of one OpenTofu provider version.",
)
