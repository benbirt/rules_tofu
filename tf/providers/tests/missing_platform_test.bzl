"""Unit tests for `missing_platform_error` in //tf/private:work_tree.bzl.

Reaching the message through the rules needs a provider with no package for
the host, so its two forms are tested here instead.
"""

load("@bazel_skylib//lib:unittest.bzl", "asserts", "unittest")
load("//tf/private:work_tree.bzl", "missing_platform_error")

_ADDRESS = "registry.opentofu.org/acme/thing"

def _tagged_test_impl(ctx):
    env = unittest.begin(ctx)

    message = missing_platform_error("//infra:prod", _ADDRESS, "1.2.3", "darwin_arm64", locked = False)

    asserts.true(env, message.startswith(
        "`//infra:prod` requires provider `registry.opentofu.org/acme/thing@1.2.3` for exec platform `darwin_arm64`",
    ), message)

    # A tag's own digests, or the registry's as recorded in the lockfile's
    # facts, under the key the extension writes them by.
    asserts.true(env, "add `darwin_arm64` to the map" in message, message)
    asserts.true(env, "delete the `acme/thing@1.2.3` entry under `tf_providers` in MODULE.bazel.lock" in message, message)
    asserts.false(env, ":pin" in message, message)

    return unittest.end(env)

_tagged_test = unittest.make(_tagged_test_impl)

def _locked_test_impl(ctx):
    env = unittest.begin(ctx)

    message = missing_platform_error("//infra:prod", _ADDRESS, "1.2.3", "darwin_arm64", locked = True)

    asserts.true(env, message.startswith(
        "`//infra:prod` requires provider `registry.opentofu.org/acme/thing@1.2.3` for exec platform `darwin_arm64`",
    ), message)

    # A locked provider records no facts and has no sha256 map: the lock is
    # the whole pin, and `pin` rewrites it.
    asserts.true(env, "bazel run @tf_providers//:pin" in message, message)
    asserts.false(env, "MODULE.bazel" in message, message)

    return unittest.end(env)

_locked_test = unittest.make(_locked_test_impl)

def missing_platform_test_suite(name):
    """Declares the `missing_platform_error` unit tests.

    Args:
        name: Name of the test_suite gathering the generated test targets.
    """
    unittest.suite(
        name,
        _tagged_test,
        _locked_test,
    )
