"""Where a pinned provider's zip is downloaded from, and what it's called.

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
# `<plugin-dir>/registry.opentofu.org/...`. It is also the registry queried
# for the download location of providers outside the `hashicorp/` namespace.
DEFAULT_HOST = "registry.opentofu.org"

# The placeholders a `url_template` may use.
_TEMPLATE_FIELDS = ["namespace", "name", "version", "os", "arch"]

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

def download_location(namespace, name, version, os, arch, url_template = ""):
    """Where to fetch one platform's provider zip from.

    In order of precedence:

      1. `url_template`, when set, is expanded and used directly.
      2. Providers in the `hashicorp/` namespace come straight from
         releases.hashicorp.com, as they always have. (OpenTofu's registry
         serves its own rebuilds of these, with different checksums, so
         switching would invalidate every existing pin.)
      3. Anything else is looked up in the OpenTofu registry, whose
         provider download endpoint returns the zip's real URL.

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
    if namespace == "hashicorp":
        return struct(
            url = ("https://releases.hashicorp.com/terraform-provider-{name}/{version}/" +
                   "terraform-provider-{name}_{version}_{os}_{arch}.zip").format(
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
        registry_url = "https://{host}/v1/providers/{ns}/{name}/{version}/download/{os}/{arch}".format(
            host = DEFAULT_HOST,
            ns = namespace,
            name = name,
            version = version,
            os = os,
            arch = arch,
        ),
        error = None,
    )

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
