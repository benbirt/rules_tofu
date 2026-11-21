"""Host platform mapping for repository rules that run `tofu`.

Repository rules run on the host and cannot resolve toolchains, so they look up
the host's pinned binary in `TOFU_BINARIES` by its `PLATFORMS` key
(`//toolchain:versions.bzl`). Kept pure so `//toolchain/tests:host_test` can
test it.
"""

_OS = {
    "linux": "linux",
    "mac os x": "darwin",
}

_ARCH = {
    "aarch64": "arm64",
    "amd64": "amd64",
    "arm64": "arm64",
    "x86_64": "amd64",
}

def host_platform_key(os_name, arch):
    """Return the `PLATFORMS` key for a host OS and architecture.

    Args:
        os_name: Value of `repository_ctx.os.name`.
        arch: Value of `repository_ctx.os.arch`.

    Returns:
        A `<os>_<arch>` key such as `linux_amd64`. The key appears in
        `PLATFORMS` only when `rules_tofu` pins an OpenTofu release for that
        host.
    """
    os_name = os_name.lower()
    os = "windows" if os_name.startswith("windows") else _OS.get(os_name, os_name.replace(" ", "_"))
    return "{}_{}".format(os, _ARCH.get(arch.lower(), arch.lower()))
