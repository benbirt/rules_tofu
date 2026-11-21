"""Unit tests for `tested_versions` in versions.bzl."""

load("@bazel_skylib//lib:unittest.bzl", "asserts", "unittest")
load(":versions.bzl", "tested_versions")

def _tested_versions_test_impl(ctx):
    env = unittest.begin(ctx)

    # Unordered, with a two-digit patch that sorts below 1.12.9 as a string.
    asserts.equals(env, ["1.12.0", "1.12.12"], tested_versions(["1.12.9", "1.12.12", "1.12.0"]))
    asserts.equals(
        env,
        ["1.12.0", "2.0.1"],
        tested_versions(["1.13.0", "1.12.9", "2.0.1", "1.12.12", "1.13.9", "1.12.0", "2.0.0"]),
    )
    asserts.equals(env, ["1.12.0"], tested_versions(["1.12.0"]))
    return unittest.end(env)

_tested_versions_test = unittest.make(_tested_versions_test_impl)

def versions_test_suite(name):
    """Declares the `tested_versions` unit tests.

    Args:
        name: Name of the test suite.
    """
    unittest.suite(name, _tested_versions_test)
