"""`tf_provider` rule.

A `tf_provider` target wraps one platform's unpacked package and optional lock
file for an OpenTofu provider version, such as `hashicorp/aws` at `5.70.0` for
`linux_amd64`.

Instantiated by the `tf_providers` module extension (see
`//tf/providers:extensions.bzl`); users do not call `tf_provider` directly.
Providers are pinned via `tf_providers.from_lock` or `tf_providers.provider` in
`MODULE.bazel` and referenced as `@tf_providers//<namespace>/<name>` in the
`providers` attribute of `tf_library`, `tf_deploy` or `tf_test`. Each generated
`tf_provider` target is visible only to `@tf_providers`, whose alias selects
the execution platform's package.
"""

load(":providers.bzl", "TfProviderInfo")

visibility(["public"])

# Subdirectory where the provider repository extracts the zip.
_PACKAGE_PREFIX = "package/"

def _tf_provider_impl(ctx):
    # Strip `package/` so `path` is relative to `<os>_<arch>/` in the plugin
    # tree.
    files = []
    for f in ctx.files.files:
        if not f.owner.name.startswith(_PACKAGE_PREFIX):
            fail("tf_provider `{}`: {} is not under {}".format(ctx.label, f.path, _PACKAGE_PREFIX))
        files.append(struct(path = f.owner.name[len(_PACKAGE_PREFIX):], file = f))

    return [
        DefaultInfo(files = depset(ctx.files.files)),
        TfProviderInfo(
            address = ctx.attr.address,
            version = ctx.attr.version,
            platform = ctx.attr.platform,
            files = files,
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
        "platform": attr.string(
            mandatory = True,
            doc = "`<os>_<arch>` platform key for this package, e.g. \"linux_amd64\".",
        ),
        "files": attr.label_list(
            mandatory = True,
            allow_files = True,
            doc = "All files from the platform's unpacked zip, including the provider binary, " +
                  "`LICENSE` and `README.md`. Each target name must begin with `package/`, which " +
                  "is stripped to form the path under `<os>_<arch>/` in the plugin tree.",
        ),
        "lock": attr.label(
            allow_single_file = True,
            doc = "Optional `.terraform.lock.hcl` from `tf_providers.from_lock`. Copied next " +
                  "to a root module's configuration before `tofu init` in validation, deploy " +
                  "runners and `tf_test`.",
        ),
    },
    provides = [TfProviderInfo],
    doc = "Wraps one platform's unpacked package and optional lock file for an OpenTofu provider version.",
)
