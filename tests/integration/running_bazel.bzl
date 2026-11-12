"""The version of the Bazel that is running the build, as a repository.

The integration tests run a Bazel inside themselves, which
rules_bazel_integration_test downloads by version. That version is this one,
so the nested builds are tested on whatever Bazel runs the tests: the pinned
one by default, and another under `USE_BAZEL_VERSION`, in CI's jobs on the
oldest supported Bazel and at a desk alike (issue #88).
"""

def _running_bazel_impl(repository_ctx):
    version = native.bazel_version

    # A Bazel built from source has no version, and so nothing to download.
    # Falling back to the pinned version would test another Bazel than the
    # one asked for, which is the bug this rule is here to prevent.
    if not version:
        fail(
            "The integration tests download the Bazel that runs them, by version, to run " +
            "inside themselves, and this Bazel has no version (it was built from source). " +
            "Run them with a released Bazel.",
        )
    repository_ctx.file("BUILD.bazel", 'exports_files(["version"])\n')

    # For `bazel_binaries.download(version_file = ...)` in MODULE.bazel.
    repository_ctx.file("version", version + "\n")

    # For //tests/integration, which passes it to every test.
    repository_ctx.file("version.bzl", 'RUNNING_BAZEL_VERSION = "{}"\n'.format(version))

running_bazel = repository_rule(
    implementation = _running_bazel_impl,
    doc = "Writes the running Bazel's version to `version` and, as `RUNNING_BAZEL_VERSION`, to `version.bzl`.",
    # Without this the rule is not run again when another Bazel takes over
    # the output base.
    local = True,
)
