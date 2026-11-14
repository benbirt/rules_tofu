"""OpenTofu toolchain type, provider and rule.

Every `rules_tofu` rule resolves `@rules_tofu//toolchain:toolchain_type` to
obtain `TofuInfo` for the execution platform.
"""

TOOLCHAIN_TYPE = "@rules_tofu//toolchain:toolchain_type"

TofuInfo = provider(
    doc = "Information about a resolved OpenTofu toolchain.",
    fields = {
        "binary": "File: the `tofu` executable.",
        "version": "string: OpenTofu version string, e.g. `\"1.12.0\"`.",
        "platform_key": "string: `<os>_<arch>` key, e.g. `\"linux_amd64\"`, used to select " +
                        "matching provider packages in each target's plugin tree.",
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
            doc = "The `tofu` executable; OpenTofu 1.12 or later is required.",
        ),
        "version": attr.string(
            mandatory = True,
            doc = "OpenTofu version string, e.g. `\"1.12.0\"`. Recorded on `TofuInfo` and not " +
                  "checked by `rules_tofu`.",
        ),
        "platform_key": attr.string(
            mandatory = True,
            doc = "`<os>_<arch>` key, e.g. `\"linux_amd64\"`, used to select matching provider " +
                  "packages in each target's plugin tree.",
        ),
    },
    doc = """Wraps an OpenTofu executable as a `rules_tofu` toolchain.

The `tofu` module extension declares one per downloaded release. A root module
may declare its own around any OpenTofu 1.12+ executable and register it with
`register_toolchains` to override the default download; see
[Bringing your own binary](https://github.com/benbirt/rules_tofu#bringing-your-own-binary).
""",
)
