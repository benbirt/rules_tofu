"""Unit tests for //tf/modules:source.bzl.

These live in a subpackage so the `@bazel_skylib` load -- a dev-only
dependency of rules_tofu -- stays out of //tf/modules:BUILD.bazel, which
downstream modules load.
"""

load("@bazel_skylib//lib:unittest.bzl", "asserts", "unittest")
load(
    "//tf/modules:source.bzl",
    "check_name",
    "check_version",
    "hub_path",
    "ls_remote_commit",
    "modules_service_url",
    "parse_source",
    "registry_download_endpoint",
    "registry_location",
    "repo_name",
    "split_subdir",
    "valid_fact",
    "version_gt",
)

_SHA = "488ab91e34a24a86957e397d9f7262ec5925586a"
_SHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

def _registry_test_impl(ctx):
    env = unittest.begin(ctx)

    parsed = parse_source("Terraform-AWS-Modules/VPC/aws")
    asserts.equals(env, None, parsed.error)
    asserts.equals(env, "registry", parsed.kind)
    asserts.equals(env, "terraform-aws-modules/vpc/aws", hub_path(parsed))
    asserts.equals(env, "tf_modules_terraform-aws-modules_vpc_aws", repo_name(hub_path(parsed)))

    other = parse_source("example.com/acme/net/aws")
    asserts.equals(env, "registry", other.kind)
    asserts.equals(env, "example.com/acme/net/aws", hub_path(other))

    with_subdir = parse_source("acme/net/aws//modules/x")
    asserts.true(env, "without `//<subdir>`" in with_subdir.error)
    return unittest.end(env)

_registry_test = unittest.make(_registry_test_impl)

def _git_test_impl(ctx):
    env = unittest.begin(ctx)

    parsed = parse_source("git::https://github.com/acme/infra.git//vpc?ref=v1.2.0")
    asserts.equals(env, None, parsed.error)
    asserts.equals(env, "git", parsed.kind)
    asserts.equals(env, "https://github.com/acme/infra.git", parsed.remote)
    asserts.equals(env, "v1.2.0", parsed.ref)
    asserts.equals(env, "vpc", parsed.subdir)

    shorthand = parse_source("github.com/acme/infra//modules/x?ref=" + _SHA)
    asserts.equals(env, "https://github.com/acme/infra.git", shorthand.remote)
    asserts.equals(env, _SHA, shorthand.ref)
    asserts.equals(env, "modules/x", shorthand.subdir)

    no_ref = parse_source("git::ssh://git@example.com/infra.git")
    asserts.equals(env, "ssh://git@example.com/infra.git", no_ref.remote)
    asserts.equals(env, "", no_ref.ref)

    asserts.true(env, "unsupported git option `sshkey`" in parse_source("git::https://e.com/x.git?sshkey=abc").error)
    asserts.true(env, "unsupported source" in parse_source("s3::https://s3.amazonaws.com/b/x.zip").error)
    return unittest.end(env)

_git_test = unittest.make(_git_test_impl)

def _archive_test_impl(ctx):
    env = unittest.begin(ctx)

    parsed = parse_source("https://example.com/vpc-1.2.0.tar.gz//vpc")
    asserts.equals(env, None, parsed.error)
    asserts.equals(env, "archive", parsed.kind)
    asserts.equals(env, "https://example.com/vpc-1.2.0.tar.gz", parsed.url)
    asserts.equals(env, "tar.gz", parsed.type)
    asserts.equals(env, "vpc", parsed.subdir)

    forced = parse_source("https://example.com/download?id=3&archive=tgz")
    asserts.equals(env, "https://example.com/download?id=3", forced.url)
    asserts.equals(env, "tar.gz", forced.type)

    asserts.true(env, "not an archive" in parse_source("https://example.com/vpc").error)
    return unittest.end(env)

_archive_test = unittest.make(_archive_test_impl)

def _split_subdir_test_impl(ctx):
    env = unittest.begin(ctx)
    for source, want in [
        ("acme/net/aws", ("acme/net/aws", "")),
        ("acme/net/aws//modules/x/", ("acme/net/aws", "modules/x")),
        ("git::https://e.com/x.git//vpc?ref=v1", ("git::https://e.com/x.git?ref=v1", "vpc")),
        ("https://e.com/x.zip", ("https://e.com/x.zip", "")),
    ]:
        asserts.equals(env, want, split_subdir(source))
    return unittest.end(env)

_split_subdir_test = unittest.make(_split_subdir_test_impl)

def _registry_api_test_impl(ctx):
    env = unittest.begin(ctx)

    service = modules_service_url("registry.opentofu.org", {"modules.v1": "/v1/modules/"})
    asserts.equals(env, "https://registry.opentofu.org/v1/modules/", service.url)
    absolute = modules_service_url("r.example.com", {"modules.v1": "https://api.example.com/mods"})
    asserts.equals(env, "https://api.example.com/mods/", absolute.url)
    asserts.true(env, "doesn't offer a module registry" in modules_service_url("x.com", {}).error)

    asserts.equals(
        env,
        "https://registry.opentofu.org/v1/modules/acme/net/aws/1.2.3/download",
        registry_download_endpoint(service.url, parse_source("acme/net/aws"), "1.2.3"),
    )
    asserts.equals(env, "git::x", registry_location({"location": "git::x"}, "m").location)
    asserts.true(env, "gave no `location`" in registry_location({}, "m").error)
    return unittest.end(env)

_registry_api_test = unittest.make(_registry_api_test_impl)

def _ls_remote_test_impl(ctx):
    env = unittest.begin(ctx)
    tag = "1" * 40
    peeled = "2" * 40
    branch = "3" * 40
    head = "4" * 40
    output = "\n".join([
        head + "\tHEAD",
        branch + "\trefs/heads/main",
        tag + "\trefs/tags/v1",
        peeled + "\trefs/tags/v1^{}",
        tag + "\trefs/tags/v2",
    ])
    asserts.equals(env, peeled, ls_remote_commit(output, "v1"))
    asserts.equals(env, tag, ls_remote_commit(output, "v2"))
    asserts.equals(env, branch, ls_remote_commit(output, "main"))
    asserts.equals(env, head, ls_remote_commit(output, ""))
    asserts.equals(env, None, ls_remote_commit(output, "v3"))
    return unittest.end(env)

_ls_remote_test = unittest.make(_ls_remote_test_impl)

def _checks_test_impl(ctx):
    env = unittest.begin(ctx)

    asserts.true(env, valid_fact({"commit": _SHA}))
    asserts.true(env, valid_fact({"location": "git::x", "commit": _SHA}))
    asserts.true(env, valid_fact({"location": "https://x.zip", "sha256": _SHA256}))
    for fact in [None, {}, {"commit": "abc"}, {"location": "git::x"}, {"commit": _SHA, "extra": 1}, ["commit"]]:
        asserts.false(env, valid_fact(fact), "accepted {}".format(fact))

    asserts.equals(env, None, check_version("5.8.1"))
    for version in ["5.8", "~> 5.8", "5.8.1-beta", ""]:
        asserts.true(env, check_version(version) != None, "accepted {}".format(repr(version)))
    asserts.true(env, version_gt("5.10.0", "5.9.9"))
    asserts.false(env, version_gt("5.9.9", "5.10.0"))

    asserts.equals(env, None, check_name("acme_vpc-1.2"))
    asserts.true(env, check_name("") != None)
    asserts.true(env, check_name("acme/vpc") != None)
    return unittest.end(env)

_checks_test = unittest.make(_checks_test_impl)

def source_test_suite(name):
    """Declares the `source.bzl` unit tests.

    Args:
        name: Name of the test_suite gathering the generated test targets.
    """
    unittest.suite(
        name,
        _registry_test,
        _git_test,
        _archive_test,
        _split_subdir_test,
        _registry_api_test,
        _ls_remote_test,
        _checks_test,
    )
