"""Unit tests for //tf/providers:source.bzl.

These live in a subpackage so the `@bazel_skylib` load -- a dev-only
dependency of rules_tofu -- stays out of //tf/providers:BUILD.bazel, which
downstream modules load.
"""

load("@bazel_skylib//lib:unittest.bzl", "asserts", "unittest")
load(
    "//tf/providers:source.bzl",
    "check_url_template",
    "download_location",
    "is_provider_binary",
    "parse_source",
    "published_platforms",
    "registry_download_url",
    "registry_shasum",
    "repo_name",
    "valid_cached_sha256s",
)

def _parse_source_lowercases_test_impl(ctx):
    env = unittest.begin(ctx)

    parsed = parse_source("PostHog/posthog")

    asserts.equals(env, None, parsed.error)
    asserts.equals(env, "posthog", parsed.namespace)
    asserts.equals(env, "posthog", parsed.name)
    asserts.equals(env, "tf_providers_posthog_posthog", repo_name(parsed.namespace, parsed.name))
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

def _registry_by_default_test_impl(ctx):
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

_registry_by_default_test = unittest.make(_registry_by_default_test_impl)

def _url_template_wins_test_impl(ctx):
    env = unittest.begin(ctx)

    # For any namespace, so a mirror can stand in for the registry.
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

_ZIP = "https://github.com/posthog/terraform-provider-posthog/releases/download/v1.0.24/terraform-provider-posthog_1.0.24_linux_amd64.zip"
_SHA = "4afb3cb829dee5e97f98b51ebdb761b8cc4daf6d0a7f541f9f5a89e0ab86251d"

def _registry_response_matching_pin_test_impl(ctx):
    env = unittest.begin(ctx)

    resolved = registry_download_url({"download_url": _ZIP, "shasum": _SHA}, _SHA, "posthog/posthog@1.0.24 on linux_amd64")

    asserts.equals(env, None, resolved.error)
    asserts.equals(env, _ZIP, resolved.url)
    return unittest.end(env)

_registry_response_matching_pin_test = unittest.make(_registry_response_matching_pin_test_impl)

def _registry_response_names_correct_pin_test_impl(ctx):
    env = unittest.begin(ctx)

    resolved = registry_download_url({"download_url": _ZIP, "shasum": _SHA}, "0" * 64, "posthog/posthog@1.0.24 on linux_amd64")

    asserts.equals(env, None, resolved.url)
    asserts.true(env, _SHA in resolved.error, "the error should name the registry's checksum: {}".format(resolved.error))
    asserts.true(env, "linux_amd64" in resolved.error)
    return unittest.end(env)

_registry_response_names_correct_pin_test = unittest.make(_registry_response_names_correct_pin_test_impl)

def _registry_response_without_url_test_impl(ctx):
    env = unittest.begin(ctx)

    resolved = registry_download_url({"errors": ["Not Found"]}, _SHA, "posthog/posthog@9.9.9 on linux_amd64")

    asserts.equals(env, None, resolved.url)
    asserts.true(env, "no download_url" in resolved.error)
    return unittest.end(env)

_registry_response_without_url_test = unittest.make(_registry_response_without_url_test_impl)

def _published_platforms_test_impl(ctx):
    env = unittest.begin(ctx)

    response = {"versions": [
        {"version": "1.0.23", "platforms": [{"os": "linux", "arch": "amd64"}]},
        {"version": "1.0.24", "platforms": [
            {"os": "windows", "arch": "amd64"},
            {"os": "linux", "arch": "amd64"},
            {"os": "freebsd", "arch": "amd64"},
            {"os": "darwin", "arch": "arm64"},
        ]},
    ]}

    # Only DEFAULT_PLATFORMS, in their order, whatever order the registry uses.
    found = published_platforms(response, "1.0.24", "posthog/posthog")
    asserts.equals(env, None, found.error)
    asserts.equals(env, ["linux_amd64", "darwin_arm64", "windows_amd64"], found.platforms)

    missing = published_platforms(response, "9.9.9", "posthog/posthog")
    asserts.true(env, "no version 9.9.9" in missing.error, "got {}".format(missing.error))

    none = published_platforms(
        {"versions": [{"version": "1.0.0", "platforms": [{"os": "freebsd", "arch": "amd64"}]}]},
        "1.0.0",
        "x/y",
    )
    asserts.true(env, "freebsd_amd64" in none.error, "got {}".format(none.error))
    return unittest.end(env)

_published_platforms_test = unittest.make(_published_platforms_test_impl)

def _registry_shasum_test_impl(ctx):
    env = unittest.begin(ctx)

    asserts.equals(env, _SHA, registry_shasum({"shasum": _SHA}, "x").sha256)
    asserts.true(env, registry_shasum({}, "x").error != None)
    asserts.true(env, registry_shasum({"shasum": "not-a-digest"}, "x").error != None)
    return unittest.end(env)

_registry_shasum_test = unittest.make(_registry_shasum_test_impl)

def _cached_facts_validated_test_impl(ctx):
    env = unittest.begin(ctx)

    asserts.true(env, valid_cached_sha256s({"linux_amd64": _SHA, "darwin_arm64": _SHA}))

    # Anything a different version of the extension might have written is a miss.
    for cached in [
        None,
        {},
        [_SHA],
        {"linux_amd64": _SHA.upper()},
        {"linux_amd64": _SHA[:-1]},
        {"freebsd_amd64": _SHA},
        {"linux_amd64": {"sha256": _SHA}},
    ]:
        asserts.false(env, valid_cached_sha256s(cached), "accepted {}".format(cached))
    return unittest.end(env)

_cached_facts_validated_test = unittest.make(_cached_facts_validated_test_impl)

def _provider_binary_names_test_impl(ctx):
    env = unittest.begin(ctx)

    # OpenTofu's builds, HashiCorp-style builds, with and without `_x5`.
    for filename, exe in [
        ("terraform-provider-aws", ""),
        ("terraform-provider-aws.exe", ".exe"),
        ("terraform-provider-aws_v6.45.0", ""),
        ("terraform-provider-aws_v6.45.0_x5", ""),
        ("terraform-provider-aws_v6.45.0_x5.exe", ".exe"),
    ]:
        asserts.true(env, is_provider_binary(filename, "aws", "6.45.0", exe), "rejected {}".format(filename))

    for filename, exe in [
        ("LICENSE", ""),
        ("CHANGELOG.md", ""),
        ("terraform-provider-aws", ".exe"),
        ("terraform-provider-aws_v6.44.0", ""),
        ("terraform-provider-awscc", ""),
        ("terraform-provider-aws.sig", ""),
    ]:
        asserts.false(env, is_provider_binary(filename, "aws", "6.45.0", exe), "accepted {}".format(filename))
    return unittest.end(env)

_provider_binary_names_test = unittest.make(_provider_binary_names_test_impl)

def source_test_suite(name):
    """Declares the `source.bzl` unit tests.

    Args:
        name: Name of the test_suite gathering the generated test targets.
    """
    unittest.suite(
        name,
        _parse_source_lowercases_test,
        _parse_source_rejects_malformed_test,
        _registry_by_default_test,
        _url_template_wins_test,
        _url_template_rejects_unknown_placeholders_test,
        _registry_response_matching_pin_test,
        _registry_response_names_correct_pin_test,
        _registry_response_without_url_test,
        _published_platforms_test,
        _registry_shasum_test,
        _cached_facts_validated_test,
        _provider_binary_names_test,
    )
