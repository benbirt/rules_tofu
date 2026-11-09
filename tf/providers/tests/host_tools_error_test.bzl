"""Unit tests for `host_tools_error` in //tf/private:host_tools.bzl.

The host tools serve `tf_providers.from_lock` first, so the test lives with
the providers' tests rather than in a dev-only package of its own.
"""

load("@bazel_skylib//lib:unittest.bzl", "asserts", "unittest")
load("//tf/private:host_tools.bzl", "host_tools_error")

_GO = "/sdk/bin/go"

def _too_old_test_impl(ctx):
    env = unittest.begin(ctx)

    message = host_tools_error(_GO, "go: go.mod requires go >= 1.25.0 (running go 1.23.5; GOTOOLCHAIN=local)\n")

    # Go's own line stays, and the hint names the SDK's origin and the fix.
    asserts.true(env, message.startswith("Could not build rules_tofu's host tools with /sdk/bin/go:\n"))
    asserts.true(env, "go.mod requires go >= 1.25.0 (running go 1.23.5; GOTOOLCHAIN=local)" in message)
    asserts.true(env, "rules_go's host Go SDK" in message)
    asserts.true(env, "go_sdk.download(version = \"...\")" in message)
    asserts.true(env, "go_sdk.from_file" in message)

    return unittest.end(env)

_too_old_test = unittest.make(_too_old_test_impl)

def _other_failure_test_impl(ctx):
    env = unittest.begin(ctx)

    stderr = "go: github.com/hashicorp/hcl/v2@v2.25.0: Get \"https://proxy.golang.org/...\": dial tcp: i/o timeout\n"
    message = host_tools_error(_GO, stderr)

    # Only Go's diagnostics, stripped, and no SDK hint.
    asserts.equals(env, "Could not build rules_tofu's host tools with /sdk/bin/go:\n" + stderr.strip(), message)
    asserts.false(env, "go_sdk" in message)

    return unittest.end(env)

_other_failure_test = unittest.make(_other_failure_test_impl)

def host_tools_error_test_suite(name):
    """Every host_tools_error test, as one suite.

    Args:
        name: The suite's name.
    """
    unittest.suite(
        name,
        _too_old_test,
        _other_failure_test,
    )
