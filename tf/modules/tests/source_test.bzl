"""Unit tests for //tf/modules:source.bzl.

These live in a subpackage because `@bazel_skylib` is a dev-only dependency
of rules_tofu and must stay out of //tf/modules:BUILD.bazel, which downstream
modules load.
"""

load("@bazel_skylib//lib:unittest.bzl", "asserts", "unittest")
load(
    "//tf/modules:source.bzl",
    "check_name",
    "check_version",
    "hub_path",
    "looks_abbreviated",
    "ls_remote_commit",
    "modules_service_url",
    "registry_download_endpoint",
    "registry_location",
    "repo_name",
    "valid_fact",
    "version_gt",
)

_SHA = "488ab91e34a24a86957e397d9f7262ec5925586a"
_SHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

def _hub_path_test_impl(ctx):
    env = unittest.begin(ctx)

    # As `modscan parse` reports a registry address, case kept.
    default = struct(host = "registry.opentofu.org", namespace = "Terraform-AWS-Modules", name = "VPC", system = "aws")
    asserts.equals(env, "terraform-aws-modules/vpc/aws", hub_path(default))
    asserts.equals(env, "tf_modules_terraform-aws-modules_vpc_aws", repo_name(hub_path(default)))

    other = struct(host = "example.com", namespace = "acme", name = "net", system = "aws")
    asserts.equals(env, "example.com/acme/net/aws", hub_path(other))
    return unittest.end(env)

_hub_path_test = unittest.make(_hub_path_test_impl)

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
        registry_download_endpoint(service.url, struct(namespace = "acme", name = "net", system = "aws"), "1.2.3"),
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

    git_fact = {"commit": _SHA}
    registry_facts = [{"location": "git::x", "commit": _SHA}, {"location": "https://x.zip", "sha256": _SHA256}]
    asserts.true(env, valid_fact(git_fact, "git"))
    for fact in registry_facts:
        asserts.true(env, valid_fact(fact, "registry"), "rejected {}".format(fact))

    # A fact of the other kind is as malformed as any (issue #96).
    asserts.false(env, valid_fact(git_fact, "registry"))
    for fact in registry_facts:
        asserts.false(env, valid_fact(fact, "git"), "accepted {} as a git fact".format(fact))
    for fact in [
        None,
        {},
        ["commit"],
        {"commit": "abc"},
        {"commit": 1},
        {"commit": _SHA, "extra": 1},
        {"commit": _SHA, 1: _SHA},
        {"location": "git::x"},
        {"location": "", "commit": _SHA},
        {"location": "git::x", "commit": "abc"},
        {"location": "https://x.zip", "sha256": "abc"},
        {"location": "https://x.zip", "sha256": _SHA256.upper()},
        {"location": "git::x", "commit": _SHA, "sha256": _SHA256},
        {"location": "git::x", "commit": _SHA, "extra": 1},
        {"sha256": _SHA256},
    ]:
        for kind in ["git", "registry"]:
            asserts.false(env, valid_fact(fact, kind), "accepted {} as a {} fact".format(fact, kind))
    asserts.false(env, valid_fact(git_fact, "archive"))

    asserts.false(env, looks_abbreviated(_SHA))
    asserts.false(env, looks_abbreviated(_SHA256))
    for ref in ["abc1234", _SHA[:39], _SHA256[:41], _SHA256[:63]]:
        asserts.true(env, looks_abbreviated(ref), "missed {}".format(ref))
    for ref in ["", "abc123", "ABC1234", "v1.2.3", "main", "feature/abc1234", _SHA + "0g"]:
        asserts.false(env, looks_abbreviated(ref), "took {} for a commit".format(repr(ref)))

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
        _hub_path_test,
        _registry_api_test,
        _ls_remote_test,
        _checks_test,
    )
