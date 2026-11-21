"""Where a provider's zips are downloaded from, and how they're checked.

Split out of `extensions.bzl` so these decisions are pure functions that
//tf/providers/tests can exercise without evaluating a module graph or
touching the network. Problems are reported as an `error` string on the
returned struct rather than raised with `fail()`, for the same reason as
//toolchain:version_resolution.bzl: only the extension and its repository
rule are entitled to abort the build.
"""

load("//toolchain:versions.bzl", "PLATFORMS")

visibility(["//tf/providers/..."])

# OpenTofu canonicalises `source = "<ns>/<name>"` in `required_providers` to
# `registry.opentofu.org/<ns>/<name>` and looks for plugins under
# `<plugin-dir>/registry.opentofu.org/...`. It is also the registry that says
# where each provider's zips are and what their SHA256s are, so rules_tofu
# vendors the same binaries a plain `tofu init` would fetch.
DEFAULT_HOST = "registry.opentofu.org"

# Supported `<os>_<arch>` platforms for providers, shared with `tofu`
# toolchain downloads. `pin` locks these by default, locked providers declare a
# repository for each, and a tag's `sha256` keys must come from this list.
DEFAULT_PLATFORMS = [platform[0] for platform in PLATFORMS]

# The placeholders a `url_template` may use.
_TEMPLATE_FIELDS = ["namespace", "name", "version", "os", "arch"]

_HEX = "0123456789abcdef"

def parse_source(source):
    """Split and normalise a `<namespace>/<name>` provider source.

    Provider addresses are case-insensitive and OpenTofu normalises them to
    lower case; `PostHog/posthog` in `required_providers` resolves to
    `registry.opentofu.org/posthog/posthog`. The vendored plugin tree and the
    repository name therefore use the lower-case form.

    Args:
        source: Provider `<namespace>/<name>` source string.

    Returns:
        `struct(namespace, name, error)`; `error` is None on success.
    """
    parts = source.split("/")
    if len(parts) != 2 or not parts[0] or not parts[1]:
        return struct(
            namespace = None,
            name = None,
            error = "provider `source` must be \"<namespace>/<name>\", got {}".format(repr(source)),
        )
    return struct(namespace = parts[0].lower(), name = parts[1].lower(), error = None)

def repo_name(namespace, name, platform):
    """Repository name for one platform's package of a provider.

    For example, `hashicorp`, `aws` and `linux_amd64` give
    `tf_providers_hashicorp_aws_linux_amd64`.
    """
    return "tf_providers_{}_{}_{}".format(namespace, name, platform)

def check_platforms(platforms):
    """Validate the `<os>_<arch>` keys of a provider tag's `sha256` map.

    Args:
        platforms: `<os>_<arch>` strings from `sha256`.

    Returns:
        An error string, or None if every key is in `DEFAULT_PLATFORMS`.
    """
    unknown = sorted([p for p in platforms if p not in DEFAULT_PLATFORMS])
    if not unknown:
        return None
    return "unsupported {} {} in `sha256`; supported platforms are {}".format(
        "platform" if len(unknown) == 1 else "platforms",
        ", ".join(["`{}`".format(p) for p in unknown]),
        ", ".join(["`{}`".format(p) for p in DEFAULT_PLATFORMS]),
    )

def missing_platform_error(source, version, platforms):
    """Format `no_match_error` for a `@tf_providers//<namespace>/<name>` alias.

    Used by the hub `select()` that chooses the execution platform's package.

    Args:
        source: Normalised `<namespace>/<name>`.
        version: Provider version.
        platforms: `<os>_<arch>` platforms the provider has repositories for.

    Returns:
        The error message.
    """
    message = ("Provider `{}@{}` has packages only for {}, not for this build's execution " +
               "platform.").format(
        source,
        version,
        ", ".join(["`{}`".format(p) for p in platforms]),
    )
    if len(platforms) < len(DEFAULT_PLATFORMS):
        message += (" Add the execution platform's zip digest to `sha256` on its " +
                    "`tf_providers.provider` tag in MODULE.bazel.")
    return message

def check_tag(sha256, url_template):
    """Validate a root module's `tf_providers.provider` tag.

    `url_template` and `sha256` are validated here rather than via `mandatory`
    on the tag class so ignored tags in dependencies always load.

    Args:
        sha256: The tag's `sha256` map.
        url_template: The tag's `url_template`.

    Returns:
        An error string, or None if the tag is valid.
    """
    if not url_template:
        return ("missing `url_template`. Pin `registry.opentofu.org` providers in `.terraform.lock.hcl` " +
                "instead: remove this tag, add `tf_providers.from_lock(lock_file = " +
                "\"//:.terraform.lock.hcl\")` and run `bazel run @tf_providers//:pin`. For an " +
                "off-registry provider, set `url_template` and `sha256`")
    error = check_url_template(url_template)
    if error:
        return error
    if not sha256:
        return "missing `sha256`: map each `<os>_<arch>` platform to its zip's SHA-256 digest"
    error = check_platforms(sha256.keys())
    if error:
        return error
    for platform, digest in sha256.items():
        if not is_sha256(digest):
            return "`sha256[\"{}\"]` must be a 64-character lower-case hex SHA-256 digest, got {}".format(platform, repr(digest))
    return None

def ignored_tag_error(source, modules):
    """Format the load-time error for a provider tagged only by dependencies.

    Args:
        source: Normalised `<namespace>/<name>`.
        modules: Names of dependency modules that tag `source`.

    Returns:
        The error message for the hub package.
    """
    return ("Provider `{source}` is tagged with `tf_providers.provider` by {modules}, but " +
            "only the root module pins providers. If `{source}` is on `registry.opentofu.org`, " +
            "declare `tf_providers.from_lock` in the root MODULE.bazel and run " +
            "`bazel run @tf_providers//:pin`; otherwise add a `tf_providers.provider` tag " +
            "there.").format(
        source = source,
        modules = ", ".join(["`{}`".format(m) for m in modules]),
    )

def registry_download_endpoint(namespace, name, version, os, arch):
    """The OpenTofu registry endpoint giving one platform's zip URL and SHA256."""
    return "https://{host}/v1/providers/{ns}/{name}/{version}/download/{os}/{arch}".format(
        host = DEFAULT_HOST,
        ns = namespace,
        name = name,
        version = version,
        os = os,
        arch = arch,
    )

def download_location(namespace, name, version, os, arch, url_template = ""):
    """Where to fetch one platform's provider zip from.

    Expands `url_template` directly for a tagged provider; otherwise returns
    the OpenTofu registry download endpoint for a locked provider.

    Args:
        namespace: Normalised provider namespace (see `parse_source`).
        name: Normalised provider type name.
        version: Provider version.
        os: Platform OS, e.g. `linux`.
        arch: Platform architecture, e.g. `amd64`.
        url_template: Optional URL template using `{namespace}`, `{name}`,
            `{version}`, `{os}` and `{arch}`.

    Returns:
        `struct(url, registry_url, error)`. On success `error` is None and
        either `url` gives the zip URL directly or `registry_url` gives the
        JSON endpoint whose `download_url` points to it.
    """
    if url_template:
        error = check_url_template(url_template)
        if error:
            return struct(url = None, registry_url = None, error = error)
        return struct(
            url = url_template.format(
                namespace = namespace,
                name = name,
                version = version,
                os = os,
                arch = arch,
            ),
            registry_url = None,
            error = None,
        )
    return struct(
        url = None,
        registry_url = registry_download_endpoint(namespace, name, version, os, arch),
        error = None,
    )

def registry_shasum(response, context):
    """Read the zip's SHA256 from an OpenTofu registry download response.

    Args:
        response: The decoded JSON body of the registry's download endpoint.
        context: A description of the provider and platform, for messages.

    Returns:
        `struct(sha256, error)`; `error` is None on success.
    """
    shasum = response.get("shasum")
    if not is_sha256(shasum):
        return struct(sha256 = None, error = "the registry's response for {} has no valid shasum".format(context))
    return struct(sha256 = shasum, error = None)

def is_sha256(value):
    """Whether `value` is a lower-case hex SHA256 digest.

    Args:
        value: Value to check; non-strings return False.

    Returns:
        A bool.
    """
    if type(value) != "string" or len(value) != 64:
        return False
    for c in value.elems():
        if c not in _HEX:
            return False
    return True

def check_url_template(url_template):
    """Check that `url_template` only uses known placeholders.

    Starlark's `format` would otherwise fail with an unhelpful message.

    Args:
        url_template: The `url_template` attribute of a provider tag.

    Returns:
        An error string, or None if the template is usable.
    """
    rest = url_template
    for _ in range(len(url_template)):
        start = rest.find("{")
        if start < 0:
            break
        end = rest.find("}", start)
        if end < 0:
            return "url_template {} has an unclosed `{{`".format(repr(url_template))
        field = rest[start + 1:end]
        if field not in _TEMPLATE_FIELDS:
            return "url_template {} uses unknown placeholder `{{{}}}`; known: {}".format(
                repr(url_template),
                field,
                ", ".join(["{" + f + "}" for f in _TEMPLATE_FIELDS]),
            )
        rest = rest[end + 1:]
    return None

def registry_download_url(response, context):
    """Read the zip's URL from an OpenTofu registry download response.

    Args:
        response: The decoded JSON body of the registry's download endpoint.
        context: A description of the provider and platform, for messages.

    Returns:
        `struct(url, error)`; `error` is None on success.
    """
    url = response.get("download_url")
    if not url:
        return struct(url = None, error = "the registry's response for {} has no download_url".format(context))
    return struct(url = url, error = None)
