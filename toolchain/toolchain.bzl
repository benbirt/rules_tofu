"""OpenTofu toolchain definition.

A `tf_deploy.<plan|apply>` runner resolves the active toolchain via
`ctx.toolchains[TOOLCHAIN_TYPE]` and reads `TofuInfo.binary` to find the
`tofu` executable to invoke at runtime.
"""

TOOLCHAIN_TYPE = "@rules_tofu//toolchain:toolchain_type"

TofuInfo = provider(
    doc = "Information about a resolved OpenTofu binary.",
    fields = {
        "binary": "File: the `tofu` executable.",
        "version": "string: the OpenTofu version, e.g. \"1.8.5\".",
        "platform_key": "string: the `<os>_<arch>` platform key the toolchain was built for. " +
                        "Build-time `tofu init` symlinks the matching `tf_provider` " +
                        "binary into the work tree's plugin dir using this key.",
    },
)

def _opentofu_toolchain_impl(ctx):
    tofu_info = TofuInfo(
        binary = ctx.file.binary,
        version = ctx.attr.version,
        platform_key = ctx.attr.platform_key,
    )
    return [
        platform_common.ToolchainInfo(tofu = tofu_info),
    ]

opentofu_toolchain = rule(
    implementation = _opentofu_toolchain_impl,
    attrs = {
        "binary": attr.label(
            allow_single_file = True,
            mandatory = True,
            doc = "The `tofu` executable.",
        ),
        "version": attr.string(
            mandatory = True,
            doc = "OpenTofu version this toolchain wraps, e.g. \"1.8.5\".",
        ),
        "platform_key": attr.string(
            mandatory = True,
            doc = "The `<os>_<arch>` platform key this toolchain targets " +
                  "(e.g. `linux_amd64`). Used by build-time `tofu init` to " +
                  "pick the matching `tf_provider` binary.",
        ),
    },
    doc = "Bundles an OpenTofu binary for use by rules_tofu rules.",
)
