"""Unit tests for //tf/providers:lock.bzl.

Parsing the file itself is tested with //tf/private/cmd/lockjson.
"""

load("@bazel_skylib//lib:unittest.bzl", "asserts", "unittest")
load("//tf/providers:lock.bzl", "lock_entries", "lock_sha256s")

_ZH_A = "3d106c7e32a929e2843f732625a582e562ff09120021e510a51a6f5d01175b8d"
_ZH_B = "1769783386610bed8bb1e861a119fe25058be41895e3996d9216dd6bb8a7aee3"

def _provider(address, version = "3.2.4", hashes = None):
    return {"address": address, "version": version, "hashes": hashes if hashes != None else ["zh:" + _ZH_A]}

def _entries_test_impl(ctx):
    env = unittest.begin(ctx)

    parsed = lock_entries([
        _provider(
            "registry.opentofu.org/hashicorp/null",
            hashes = ["h1:346niW/SBt/jtCZctHefjkQGqtX9YgxSkCAKeCzQhXw=", "zh:" + _ZH_A, "zh:" + _ZH_B],
        ),
        _provider("registry.opentofu.org/PostHog/PostHog", version = "1.0.24"),
    ])

    asserts.equals(env, None, parsed.error)
    asserts.equals(env, 2, len(parsed.entries))
    null = parsed.entries[0]
    asserts.equals(env, "hashicorp/null", null.source)
    asserts.equals(env, "hashicorp", null.namespace)
    asserts.equals(env, "null", null.name)
    asserts.equals(env, "3.2.4", null.version)
    asserts.equals(env, [_ZH_A, _ZH_B], null.zh)
    asserts.equals(env, None, null.error)
    asserts.equals(env, "posthog/posthog", parsed.entries[1].source)
    return unittest.end(env)

_entries_test = unittest.make(_entries_test_impl)

def _empty_lock_test_impl(ctx):
    env = unittest.begin(ctx)

    parsed = lock_entries([])
    asserts.equals(env, None, parsed.error)
    asserts.equals(env, [], parsed.entries)
    return unittest.end(env)

_empty_lock_test = unittest.make(_empty_lock_test_impl)

def _unusable_lock_test_impl(ctx):
    env = unittest.begin(ctx)

    cases = [
        ([_provider("a/b")], "is not `<host>/<namespace>/<name>`"),
        ([_provider("registry.opentofu.org/a/b", version = "")], "has no `version`"),
        (
            [_provider("registry.opentofu.org/a/b"), _provider("registry.opentofu.org/A/B", version = "2")],
            "locked more than once",
        ),
    ]
    for providers, fragment in cases:
        parsed = lock_entries(providers)
        asserts.equals(env, None, parsed.entries)
        asserts.true(
            env,
            parsed.error != None and fragment in parsed.error,
            "expected {} in the error for {}, got {}".format(repr(fragment), providers, repr(parsed.error)),
        )
    return unittest.end(env)

_unusable_lock_test = unittest.make(_unusable_lock_test_impl)

def _unusable_entries_test_impl(ctx):
    env = unittest.begin(ctx)

    # Locked by `terraform`, against HashiCorp's registry and builds.
    terraform = lock_entries([_provider("registry.terraform.io/hashicorp/null")])
    asserts.equals(env, None, terraform.error)
    asserts.true(env, "registry.terraform.io" in terraform.entries[0].error)
    asserts.true(env, "Lock it with OpenTofu" in terraform.entries[0].error)

    # Only `h1:` hashes: nothing to check a zip against.
    h1_only = lock_entries([_provider(
        "registry.opentofu.org/hashicorp/null",
        hashes = ["h1:346niW/SBt/jtCZctHefjkQGqtX9YgxSkCAKeCzQhXw="],
    )])
    asserts.true(env, "no `zh:` hashes" in h1_only.entries[0].error)
    return unittest.end(env)

_unusable_entries_test = unittest.make(_unusable_entries_test_impl)

def _lock_sha256s_test_impl(ctx):
    env = unittest.begin(ctx)

    vouched = lock_sha256s({"linux_amd64": _ZH_A, "darwin_arm64": _ZH_B}, [_ZH_B, _ZH_A], "hashicorp/null@3.2.4")
    asserts.equals(env, None, vouched.error)
    asserts.equals(env, {"linux_amd64": _ZH_A, "darwin_arm64": _ZH_B}, vouched.sha256s)

    mismatch = lock_sha256s({"linux_amd64": _ZH_A, "darwin_arm64": _ZH_B}, [_ZH_A], "hashicorp/null@3.2.4")
    asserts.equals(env, None, mismatch.sha256s)
    asserts.true(env, "hashicorp/null@3.2.4 on darwin_arm64 matches none" in mismatch.error)
    return unittest.end(env)

_lock_sha256s_test = unittest.make(_lock_sha256s_test_impl)

def lock_test_suite(name):
    """Declares the `lock.bzl` unit tests.

    Args:
        name: Name of the test suite.
    """
    unittest.suite(
        name,
        _entries_test,
        _empty_lock_test,
        _unusable_lock_test,
        _unusable_entries_test,
        _lock_sha256s_test,
    )
