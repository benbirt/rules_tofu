"""Unit tests for `host_platform_key` in //toolchain:host.bzl."""

load("@bazel_skylib//lib:unittest.bzl", "asserts", "unittest")
load("//toolchain:host.bzl", "host_platform_key")
load("//toolchain:versions.bzl", "PLATFORMS")

def _host_platform_key_test_impl(ctx):
    env = unittest.begin(ctx)

    # Bazel populates `repository_ctx.os.name` and `.arch` from the JVM's
    # os.name and os.arch in lower case.
    hosts = {
        ("linux", "amd64"): "linux_amd64",
        ("linux", "aarch64"): "linux_arm64",
        ("mac os x", "x86_64"): "darwin_amd64",
        ("mac os x", "aarch64"): "darwin_arm64",
        ("Mac OS X", "arm64"): "darwin_arm64",
        ("windows 11", "amd64"): "windows_amd64",
        ("windows server 2022", "amd64"): "windows_amd64",
    }
    keys = [platform[0] for platform in PLATFORMS]
    for (os_name, arch), want in hosts.items():
        got = host_platform_key(os_name, arch)
        asserts.equals(env, want, got)
        asserts.true(env, got in keys, "{} is not in PLATFORMS".format(got))

    # Hosts without a pinned release still get a `<os>_<arch>` key outside
    # PLATFORMS so the caller can name the host in its error message.
    for (os_name, arch), want in {
        ("linux", "s390x"): "linux_s390x",
        ("freebsd", "amd64"): "freebsd_amd64",
        ("windows 11", "aarch64"): "windows_arm64",
    }.items():
        got = host_platform_key(os_name, arch)
        asserts.equals(env, want, got)
        asserts.false(env, got in keys, "{} is in PLATFORMS".format(got))
    return unittest.end(env)

_host_platform_key_test = unittest.make(_host_platform_key_test_impl)

def host_test_suite(name):
    unittest.suite(name, _host_platform_key_test)
