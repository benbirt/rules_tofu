"""Unit tests for //tf/providers:lock.bzl."""

load("@bazel_skylib//lib:unittest.bzl", "asserts", "unittest")
load("//tf/providers:lock.bzl", "lock_sha256s", "parse_lock")

_ZH_A = "3d106c7e32a929e2843f732625a582e562ff09120021e510a51a6f5d01175b8d"
_ZH_B = "1769783386610bed8bb1e861a119fe25058be41895e3996d9216dd6bb8a7aee3"

# As `tofu providers lock` writes it.
_TOFU_LOCK = """\
# This file is maintained automatically by "tofu init".
# Manual edits may be lost in future updates.

provider "registry.opentofu.org/hashicorp/null" {
  version     = "3.2.4"
  constraints = "3.2.4"
  hashes = [
    "h1:346niW/SBt/jtCZctHefjkQGqtX9YgxSkCAKeCzQhXw=",
    "zh:%s",
    "zh:%s",
  ]
}

provider "registry.opentofu.org/posthog/posthog" {
  version = "1.0.24"
  hashes = ["zh:%s"]
}
""" % (_ZH_A, _ZH_B, _ZH_B)

def _parses_tofu_lock_test_impl(ctx):
    env = unittest.begin(ctx)

    parsed = parse_lock(_TOFU_LOCK)

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

_parses_tofu_lock_test = unittest.make(_parses_tofu_lock_test_impl)

def _tolerates_comments_and_unknown_attributes_test_impl(ctx):
    env = unittest.begin(ctx)

    parsed = parse_lock("""
// Written by some other tool.
/* A block
   comment. */
provider "registry.opentofu.org/PostHog/PostHog" {
  version  = "1.0.24" # trailing comment
  future   = "ignored"
  hashes   = ["zh:%s"]
}
""" % _ZH_A)

    asserts.equals(env, None, parsed.error)
    asserts.equals(env, "posthog/posthog", parsed.entries[0].source)
    asserts.equals(env, [_ZH_A], parsed.entries[0].zh)
    return unittest.end(env)

_tolerates_comments_and_unknown_attributes_test = unittest.make(_tolerates_comments_and_unknown_attributes_test_impl)

def _empty_lock_test_impl(ctx):
    env = unittest.begin(ctx)

    for text in ["", "# nothing locked yet\n"]:
        parsed = parse_lock(text)
        asserts.equals(env, None, parsed.error)
        asserts.equals(env, [], parsed.entries)
    return unittest.end(env)

_empty_lock_test = unittest.make(_empty_lock_test_impl)

def _malformed_lock_test_impl(ctx):
    env = unittest.begin(ctx)

    cases = {
        "module \"x\" {}": "expected a `provider` block",
        "provider registry {}": "expected the provider address",
        "provider \"registry.opentofu.org/a/b\" {\n  version = \"1\"\n": "is not closed",
        "provider \"registry.opentofu.org/a/b\" { version = \"1\n\" }": "unterminated string",
        "provider \"registry.opentofu.org/a/b\" { hashes = [\"zh:x\" \"zh:y\"] }": "expected `,` or `]`",
        "provider \"a/b\" { version = \"1\" }": "is not `<host>/<namespace>/<name>`",
        "provider \"registry.opentofu.org/a/b\" { hashes = [] }": "has no `version`",
        "provider \"registry.opentofu.org/a/b\" { version = \"1\" }\nprovider \"registry.opentofu.org/A/B\" { version = \"2\" }": "locked more than once",
        "provider \"registry.opentofu.org/a/b\" { version = 1 }": "expected a string or a list for `version`",
        "provider \"registry.opentofu.org/a/b\" { version = @ }": "line 1: unexpected character",
    }
    for text, fragment in cases.items():
        parsed = parse_lock(text)
        asserts.equals(env, None, parsed.entries, "expected no entries for {}".format(repr(text)))
        asserts.true(
            env,
            parsed.error != None and fragment in parsed.error,
            "expected {} in the error for {}, got {}".format(repr(fragment), repr(text), repr(parsed.error)),
        )
    return unittest.end(env)

_malformed_lock_test = unittest.make(_malformed_lock_test_impl)

def _unusable_entries_test_impl(ctx):
    env = unittest.begin(ctx)

    # Locked by `terraform`, against HashiCorp's registry and builds.
    terraform = parse_lock("""
provider "registry.terraform.io/hashicorp/null" {
  version = "3.2.4"
  hashes  = ["zh:%s"]
}
""" % _ZH_A)
    asserts.equals(env, None, terraform.error)
    asserts.true(env, "registry.terraform.io" in terraform.entries[0].error)
    asserts.true(env, "Lock it with OpenTofu" in terraform.entries[0].error)

    # Only `h1:` hashes: nothing to check a zip against.
    h1_only = parse_lock("""
provider "registry.opentofu.org/hashicorp/null" {
  version = "3.2.4"
  hashes  = ["h1:346niW/SBt/jtCZctHefjkQGqtX9YgxSkCAKeCzQhXw="]
}
""")
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
        _parses_tofu_lock_test,
        _tolerates_comments_and_unknown_attributes_test,
        _empty_lock_test,
        _malformed_lock_test,
        _unusable_entries_test,
        _lock_sha256s_test,
    )
