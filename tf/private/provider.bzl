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
    # Each file sits at `plugins/<address>/<version>/<os>_<arch>/<path>` in
    # the provider's repo; the plugin tree wants `<path>`.
    files = {}
    for tgt, platform_key in ctx.attr.files.items():
        prefix = "plugins/{}/{}/{}/".format(ctx.attr.address, ctx.attr.version, platform_key)
        for f in tgt.files.to_list():
            if not f.owner.name.startswith(prefix):
                fail("tf_provider `{}`: {} is not under {}".format(ctx.label, f.path, prefix))
            files.setdefault(platform_key, []).append(
                struct(path = f.owner.name[len(prefix):], file = f),
            )

    return [
        DefaultInfo(files = depset([entry.file for entries in files.values() for entry in entries])),
        TfProviderInfo(
            address = ctx.attr.address,
            version = ctx.attr.version,
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
        "files": attr.label_keyed_string_dict(
            mandatory = True,
            allow_files = True,
            doc = "Map of every file from each platform's unpacked zip, the provider binary and " +
                  "files such as `LICENSE` and `README.md` alike, to `<os>_<arch>` platform key. " +
                  "Each file's target name must start with " +
                  "`plugins/<address>/<version>/<os>_<arch>/`, which is stripped to give its path " +
                  "inside `<os>_<arch>/` in the plugin tree.",
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
