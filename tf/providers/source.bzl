"""Where a provider's zips are downloaded from, and how they're checked.

Split out of `extensions.bzl` so these decisions are pure functions that
//tf/providers/tests can exercise without evaluating a module graph or
touching the network. Problems are reported as an `error` string on the
returned struct rather than raised with `fail()`, for the same reason as
//toolchain:version_resolution.bzl: only the extension and its repository
rule are entitled to abort the build.
"""

visibility(["//tf/providers/..."])

# OpenTofu canonicalizes `source = "<ns>/<name>"` in `required_providers` to
# `registry.opentofu.org/<ns>/<name>` and looks for plugins under
# `<plugin-dir>/registry.opentofu.org/...`. It is also the registry that says
# where each provider's zips are and what their SHA256s are, so rules_tofu
# vendors the same binaries a plain `tofu init` would fetch.
DEFAULT_HOST = "registry.opentofu.org"

# The platforms resolved for a provider tag that pins no `sha256`, where the
# registry publishes them: the hosts Bazel and OpenTofu commonly run on.
DEFAULT_PLATFORMS = ["linux_amd64", "linux_arm64", "darwin_amd64", "darwin_arm64", "windows_amd64"]

# The placeholders a `url_template` may use.
_TEMPLATE_FIELDS = ["namespace", "name", "version", "os", "arch"]

_HEX = "0123456789abcdef"

def parse_source(source):
    """Split and normalize a `<namespace>/<name>` provider source.

    Provider addresses are case-insensitive and OpenTofu normalizes them to
    lower case (so `PostHog/posthog` in `required_providers` resolves to
    `registry.opentofu.org/posthog/posthog`). The vendored plugin tree and the
    repository name therefore use the lower-case form.

    Args:
        source: The `source` attribute of a `tf_providers.provider` tag.

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

def repo_name(namespace, name):
    """The repository a provider is vendored into.

    `hashicorp/aws` → `tf_providers_hashicorp_aws`.
    """
    return "tf_providers_{}_{}".format(namespace, name)

def registry_download_endpoint(namespace, name, version, os, arch):
    """The OpenTofu registry endpoint describing one platform's zip (URL and SHA256)."""
    return "https://{host}/v1/providers/{ns}/{name}/{version}/download/{os}/{arch}".format(
        host = DEFAULT_HOST,
        ns = namespace,
        name = name,
        version = version,
        os = os,
        arch = arch,
    )

def registry_versions_endpoint(namespace, name):
    """The OpenTofu registry endpoint listing a provider's versions and their platforms."""
    return "https://{host}/v1/providers/{ns}/{name}/versions".format(
        host = DEFAULT_HOST,
        ns = namespace,
        name = name,
    )

def download_location(namespace, name, version, os, arch, url_template = ""):
    """Where to fetch one platform's provider zip from.

    `url_template`, when set, is expanded and used directly. Otherwise the
    OpenTofu registry's download endpoint says where the zip is.

    Args:
        namespace: Normalized provider namespace (see `parse_source`).
        name: Normalized provider type name.
        version: Provider version.
        os: Platform OS, e.g. `linux`.
        arch: Platform architecture, e.g. `amd64`.
        url_template: Optional template using `{namespace}`, `{name}`,
            `{version}`, `{os}` and `{arch}`.

    Returns:
        `struct(url, registry_url, error)`. Exactly one of `url` (download
        the zip from here) and `registry_url` (fetch this JSON and read its
        `download_url`) is set on success; `error` is None.
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

def published_platforms(versions_response, version, context):
    """The `DEFAULT_PLATFORMS` the registry publishes for `version`.

    Args:
        versions_response: The decoded JSON body of the registry's versions endpoint.
        version: The provider version wanted.
        context: A description of the provider, for messages.

    Returns:
        `struct(platforms, error)`: platform keys in `DEFAULT_PLATFORMS` order;
        `error` is None on success.
    """
    for entry in versions_response.get("versions", []):
        if entry.get("version") != version:
            continue
        published = ["{}_{}".format(p.get("os"), p.get("arch")) for p in entry.get("platforms", [])]
        platforms = [p for p in DEFAULT_PLATFORMS if p in published]
        if not platforms:
            return struct(
                platforms = None,
                error = "the registry publishes {} for none of {} (only {})".format(
                    context,
                    ", ".join(DEFAULT_PLATFORMS),
                    ", ".join(published) or "nothing",
                ),
            )
        return struct(platforms = platforms, error = None)
    return struct(platforms = None, error = "the registry has no version {} of {}".format(version, context))

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
    """Whether `value` is a lower-case hex SHA256 digest."""
    if type(value) != "string" or len(value) != 64:
        return False
    for c in value.elems():
        if c not in _HEX:
            return False
    return True

def valid_cached_sha256s(cached):
    """Whether a value read back from the lockfile's facts is a usable sha256 map.

    Facts can outlive the code that wrote them, so anything unexpected is
    treated as a cache miss rather than trusted.

    Args:
        cached: A value from `module_ctx.facts`, or None.

    Returns:
        True if `cached` maps known platform keys to SHA256 digests.
    """
    if type(cached) != "dict" or not cached:
        return False
    for platform, digest in cached.items():
        if platform not in DEFAULT_PLATFORMS or not is_sha256(digest):
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

def registry_download_url(response, pinned_sha256, context):
    """Read the zip's URL from an OpenTofu registry download response.

    The registry also reports the zip's SHA256. Checking it against the pin
    up front turns a wrong or missing pin into an error naming the right
    value, rather than a bare checksum mismatch after a long download.

    Args:
        response: The decoded JSON body of the registry's download endpoint.
        pinned_sha256: The SHA256 declared on the provider tag for this platform.
        context: A description of the provider and platform, for messages.

    Returns:
        `struct(url, error)`; `error` is None on success.
    """
    url = response.get("download_url")
    if not url:
        return struct(url = None, error = "the registry's response for {} has no download_url".format(context))
    shasum = response.get("shasum")
    if shasum and shasum != pinned_sha256:
        return struct(
            url = None,
            error = ("sha256 for {} is pinned as {}, but the registry reports {} for {}. " +
                     "If you're adding or updating this provider, pin the registry's value.").format(
                context,
                pinned_sha256,
                shasum,
                url,
            ),
        )
    return struct(url = url, error = None)

def is_provider_binary(filename, name, version, exe_suffix):
    """Whether `filename`, from a provider zip, is the provider binary.

    HashiCorp-style builds (and most publishers') name it
    `terraform-provider-<name>_v<version>` with an optional `_x5` protocol
    suffix; OpenTofu's own builds of the `hashicorp/` providers name it plain
    `terraform-provider-<name>`. OpenTofu accepts either in a plugin
    directory. Anything else in the zip (LICENSE, README.md, CHANGELOG.md) is
    not the binary.

    Args:
        filename: A file name from the extracted zip.
        name: Normalized provider type name.
        version: Provider version.
        exe_suffix: `.exe` on Windows, else empty.

    Returns:
        True if `filename` is the binary.
    """
    if not filename.endswith(exe_suffix):
        return False
    stem = filename[:len(filename) - len(exe_suffix)] if exe_suffix else filename
    plain = "terraform-provider-" + name
    return stem == plain or stem.startswith("{}_v{}".format(plain, version))
