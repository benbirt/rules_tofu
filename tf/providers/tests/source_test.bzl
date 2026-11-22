"""Unit tests for //tf/providers:source.bzl.

These live in a subpackage because `@bazel_skylib` is a dev-only dependency
of rules_tofu and must stay out of //tf/providers:BUILD.bazel, which
downstream modules load.
"""

load("@bazel_skylib//lib:unittest.bzl", "asserts", "unittest")
load(
    "//tf/providers:source.bzl",
    "DEFAULT_PLATFORMS",
    "check_tag",
    "check_url_template",
    "download_location",
    "ignored_tag_error",
    "missing_platform_error",
    "parse_source",
    "registry_shasum",
    "repo_name",
)

def _parse_source_lowercases_test_impl(ctx):
    env = unittest.begin(ctx)

    parsed = parse_source("PostHog/posthog")

    asserts.equals(env, None, parsed.error)
    asserts.equals(env, "posthog", parsed.namespace)
    asserts.equals(env, "posthog", parsed.name)
    asserts.equals(
        env,
        "tf_providers_posthog_posthog_linux_amd64",
        repo_name(parsed.namespace, parsed.name, "linux_amd64"),
    )
    return unittest.end(env)

_parse_source_lowercases_test = unittest.make(_parse_source_lowercases_test_impl)

def _parse_source_rejects_malformed_test_impl(ctx):
    env = unittest.begin(ctx)

    for source in ["aws", "hashicorp/", "/aws", "registry.opentofu.org/hashicorp/aws"]:
        parsed = parse_source(source)
        asserts.true(env, parsed.error != None, "expected an error for {}".format(repr(source)))
        asserts.true(env, "<namespace>/<name>" in parsed.error)
    return unittest.end(env)

_parse_source_rejects_malformed_test = unittest.make(_parse_source_rejects_malformed_test_impl)

def _registry_for_locked_test_impl(ctx):
    env = unittest.begin(ctx)

    location = download_location("posthog", "posthog", "1.0.24", "darwin", "arm64")

    asserts.equals(env, None, location.error)
    asserts.equals(env, None, location.url)
    asserts.equals(
        env,
        "https://registry.opentofu.org/v1/providers/posthog/posthog/1.0.24/download/darwin/arm64",
        location.registry_url,
    )

    # hashicorp/ too: the registry serves OpenTofu's builds, as `tofu init` uses.
    hashicorp = download_location("hashicorp", "aws", "6.45.0", "linux", "amd64")
    asserts.equals(env, None, hashicorp.url)
    asserts.equals(
        env,
        "https://registry.opentofu.org/v1/providers/hashicorp/aws/6.45.0/download/linux/amd64",
        hashicorp.registry_url,
    )
    return unittest.end(env)

_registry_for_locked_test = unittest.make(_registry_for_locked_test_impl)

def _url_template_wins_test_impl(ctx):
    env = unittest.begin(ctx)

    # url_template applies to any namespace, including hashicorp/.
    location = download_location(
        "hashicorp",
        "aws",
        "6.45.0",
        "linux",
        "arm64",
        "https://mirror.example/{namespace}/{name}/{version}/{name}_{version}_{os}_{arch}.zip",
    )

    asserts.equals(env, None, location.error)
    asserts.equals(env, None, location.registry_url)
    asserts.equals(env, "https://mirror.example/hashicorp/aws/6.45.0/aws_6.45.0_linux_arm64.zip", location.url)
    return unittest.end(env)

_url_template_wins_test = unittest.make(_url_template_wins_test_impl)

def _url_template_rejects_unknown_placeholders_test_impl(ctx):
    env = unittest.begin(ctx)

    asserts.equals(env, None, check_url_template("https://example.com/{name}_{version}_{os}_{arch}.zip"))
    asserts.equals(env, None, check_url_template("https://example.com/fixed.zip"))

    unknown = check_url_template("https://example.com/{name}_{platform}.zip")
    asserts.true(env, unknown != None and "{platform}" in unknown, "got {}".format(unknown))

    unclosed = check_url_template("https://example.com/{name.zip")
    asserts.true(env, unclosed != None and "unclosed" in unclosed, "got {}".format(unclosed))

    location = download_location("posthog", "posthog", "1.0.24", "linux", "amd64", "https://x/{arch")
    asserts.equals(env, None, location.url)
    asserts.true(env, location.error != None)
    return unittest.end(env)

_url_template_rejects_unknown_placeholders_test = unittest.make(_url_template_rejects_unknown_placeholders_test_impl)

_SHA = "4afb3cb829dee5e97f98b51ebdb761b8cc4daf6d0a7f541f9f5a89e0ab86251d"

def _registry_shasum_test_impl(ctx):
    env = unittest.begin(ctx)

    asserts.equals(env, _SHA, registry_shasum({"shasum": _SHA}, "x").sha256)
    asserts.true(env, registry_shasum({}, "x").error != None)
    asserts.true(env, registry_shasum({"shasum": "not-a-digest"}, "x").error != None)
    return unittest.end(env)

_registry_shasum_test = unittest.make(_registry_shasum_test_impl)

def _missing_platform_test_impl(ctx):
    env = unittest.begin(ctx)

    # Partial platform map (from a tag's sha256): suggest adding the digest.
    partial = missing_platform_error("hashicorp/aws", "6.45.0", ["linux_amd64", "darwin_arm64"])
    asserts.true(env, partial.startswith("Provider `hashicorp/aws@6.45.0` has packages only for `linux_amd64`, `darwin_arm64`, not for"), partial)
    asserts.true(env, partial.endswith("Add the execution platform's zip digest to `sha256` on its `tf_providers.provider` tag in MODULE.bazel."), partial)

    # All supported platforms present: no sha256 entry can add another.
    full = missing_platform_error("hashicorp/null", "3.2.4", DEFAULT_PLATFORMS)
    asserts.true(env, full.endswith("not for this build's execution platform."), full)
    return unittest.end(env)

_missing_platform_test = unittest.make(_missing_platform_test_impl)

_TEMPLATE = "https://example.com/{name}_{version}_{os}_{arch}.zip"

def _check_tag_test_impl(ctx):
    env = unittest.begin(ctx)

    asserts.equals(env, None, check_tag({"linux_amd64": _SHA, "darwin_arm64": _SHA}, _TEMPLATE))

    # A 0.5 registry-provider tag without url_template (with or without sha256)
    # points to from_lock and pin.
    for sha256 in [{}, {"linux_amd64": _SHA}]:
        registry = check_tag(sha256, "")
        asserts.true(env, registry.startswith("missing `url_template`."), registry)
        asserts.true(env, "tf_providers.from_lock(lock_file = \"//:.terraform.lock.hcl\")" in registry, registry)
        asserts.true(env, "bazel run @tf_providers//:pin" in registry, registry)

    asserts.true(env, check_tag({}, _TEMPLATE).startswith("missing `sha256`"))
    asserts.true(env, "unknown placeholder `{platform}`" in check_tag({"linux_amd64": _SHA}, "https://x/{platform}"))
    asserts.true(env, "unsupported platform `linux_386`" in check_tag({"linux_386": _SHA}, _TEMPLATE))
    asserts.true(env, "unsupported platforms `linux_386`, `windows_386` in" in check_tag({"linux_amd64": _SHA, "windows_386": _SHA, "linux_386": _SHA}, _TEMPLATE))
    asserts.true(env, "`sha256[\"linux_amd64\"]` must be" in check_tag({"linux_amd64": "sha256:" + _SHA}, _TEMPLATE))
    return unittest.end(env)

_check_tag_test = unittest.make(_check_tag_test_impl)

def _ignored_tag_test_impl(ctx):
    env = unittest.begin(ctx)

    one = ignored_tag_error("acme/widgets", ["lib"])
    asserts.true(env, one.startswith("Provider `acme/widgets` is tagged with `tf_providers.provider` by `lib`, but only the root module pins providers."), one)
    asserts.true(env, "bazel run @tf_providers//:pin" in one, one)

    two = ignored_tag_error("acme/widgets", ["lib", "net"])
    asserts.true(env, "by `lib`, `net`, but" in two, two)
    return unittest.end(env)

_ignored_tag_test = unittest.make(_ignored_tag_test_impl)

def source_test_suite(name):
    """Declares the `source.bzl` unit tests.

    Args:
        name: Name of the test_suite gathering the generated test targets.
    """
    unittest.suite(
        name,
        _missing_platform_test,
        _parse_source_lowercases_test,
        _parse_source_rejects_malformed_test,
        _registry_for_locked_test,
        _url_template_wins_test,
        _url_template_rejects_unknown_placeholders_test,
        _registry_shasum_test,
        _check_tag_test,
        _ignored_tag_test,
    )
