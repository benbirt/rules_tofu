"""Pinned OpenTofu release metadata.

To pin a new release, run `bazel run //tools:update_versions -- <version>`, or
pass `--default` to update `DEFAULT_VERSION` as well. The script verifies the
cosign signature on `SHA256SUMS` before copying the digests here.

`rules_tofu` downloads OpenTofu release binaries by design rather than building
them from source. The pinned download is already hermetic: content-addressed,
cached by Bazel and offline after the first fetch. A source build would pull
roughly 300 Go modules into every downstream module graph through `go_deps`, and
as a non-root module `rules_tofu` could not apply OpenTofu's `replace` of
`hashicorp/hcl`. A root module that wants a source build can register its own
toolchain; see "The `tofu` binary" in `README.md`.
"""

# OpenTofu 1.12 is the oldest release rules_tofu supports because the rules pass
# flags that older releases lack, such as `tofu test -json-into` from 1.12.0.
# Pin nothing older here.
DEFAULT_VERSION = "1.12.0"

# Version -> { "<os>_<arch>": "<sha256>" }. Keys match the <os>_<arch> suffix
# of OpenTofu's release archives, tofu_<version>_<os>_<arch>.zip; PLATFORMS
# maps each key to Bazel's OS and CPU constraints.
KNOWN_VERSIONS = {
    "1.12.0": {
        "linux_amd64": "8d7650fd42b6d790f9f747604393ccd0a9035376bccc4f1688b905d7c5bb1137",
        "linux_arm64": "466bf912404b4ab0f0b3a043073d68ad34f11d55ad7a483957d94f0733169f8d",
        "darwin_amd64": "761dc6688325721be230f95b94382bc06ffe59d87cb25c94ef8a37d9cb0c0014",
        "darwin_arm64": "1b09890dc4ed842bebb55b8c958943b28bc025b3728ee2e5f848c30ee3406841",
        "windows_amd64": "7253abf6ce9c0e88e0cc188c5c883e02353b6c5ffcf2125e6c307348ca223df0",
    },
}

# (platform_key, bazel_os_constraint, bazel_cpu_constraint, exe_suffix)
PLATFORMS = [
    ("linux_amd64", "@platforms//os:linux", "@platforms//cpu:x86_64", ""),
    ("linux_arm64", "@platforms//os:linux", "@platforms//cpu:aarch64", ""),
    ("darwin_amd64", "@platforms//os:macos", "@platforms//cpu:x86_64", ""),
    ("darwin_arm64", "@platforms//os:macos", "@platforms//cpu:aarch64", ""),
    ("windows_amd64", "@platforms//os:windows", "@platforms//cpu:x86_64", ".exe"),
]
