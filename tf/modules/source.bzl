"""Where `tf_modules.module` fetches a module from, and what pins it.

Split out of `extensions.bzl` so these decisions are pure functions that
//tf/modules/tests can exercise without evaluating a module graph or touching
the network. Problems are reported as an `error` string on the returned
struct rather than raised with `fail()`: only the extension and its
repository rule are entitled to abort the build.

Reading a module's `source` is not done here: `modscan parse` does it in Go
(//tf/private/cmd/internal/tfconfig), with OpenTofu's own registry-address
library, so rules_tofu reads addresses as OpenTofu does. `parsed` below is
what `parse_sources` in `extensions.bzl` makes of its answer.
"""

visibility(["//tf/modules/..."])

DEFAULT_HOST = "registry.opentofu.org"

_HEX = "0123456789abcdef"

def hub_path(parsed):
    """A registry module's package in `@tf_modules`.

    `<namespace>/<name>/<system>` in lower case, prefixed with the host unless
    it is the default one.
    """
    path = "{}/{}/{}".format(parsed.namespace, parsed.name, parsed.system).lower()
    if parsed.host != DEFAULT_HOST:
        path = parsed.host + "/" + path
    return path

def check_name(name):
    """Return an error if `name` can't be a module's package in `@tf_modules`, else None."""
    if not name:
        return "must be set for a git or archive source, as the module's label `@tf_modules//<name>`"
    for c in name.elems():
        if not (c.isalnum() or c in "_-."):
            return "`{}` may only contain letters, digits, `_`, `-` and `.`".format(name)
    return None

def repo_name(path):
    """The repository holding a module: `tf_modules_` plus its hub path, `/` as `_`."""
    return "tf_modules_" + path.replace("/", "_")

def registry_download_endpoint(modules_url, parsed, version):
    """The registry endpoint giving the location of a module version's source."""
    return "{}{}/{}/{}/{}/download".format(modules_url, parsed.namespace, parsed.name, parsed.system, version)

def modules_service_url(host, discovery):
    """The base URL of a registry's modules API, from its `/.well-known/terraform.json`.

    Returns:
        `struct(url, error)`; `url` ends with `/`.
    """
    path = discovery.get("modules.v1") if type(discovery) == "dict" else None
    if type(path) != "string" or not path:
        return struct(url = None, error = "{} doesn't offer a module registry (no `modules.v1` in /.well-known/terraform.json)".format(host))
    if not path.startswith("https://"):
        path = "https://" + host + "/" + path.lstrip("/")
    if not path.endswith("/"):
        path += "/"
    return struct(url = path, error = None)

def registry_location(response, context):
    """The source location in a registry's download response.

    Returns:
        `struct(location, error)`.
    """
    location = response.get("location") if type(response) == "dict" else None
    if type(location) != "string" or not location:
        return struct(location = None, error = ("The registry gave no `location` for {} in its response body. " +
                                                "rules_tofu can only use registries that do, such as {}.").format(context, DEFAULT_HOST))
    return struct(location = location, error = None)

def is_commit(ref):
    """Whether `ref` is a full git commit ID (SHA-1 or SHA-256)."""
    return len(ref) in (40, 64) and all([c in _HEX for c in ref.elems()])

def looks_abbreviated(ref):
    """Whether `ref` could be a commit ID cut short: 7 or more hex characters, but not all of one."""
    return len(ref) >= 7 and len(ref) < 64 and len(ref) != 40 and all([c in _HEX for c in ref.lower().elems()])

def ls_remote_commit(output, ref):
    """The commit `git ls-remote` reports for `ref`, or None.

    A tag's peeled entry (`^{}`, an annotated tag's commit) wins over the tag
    itself, and tags over branches. An empty `ref` is the remote's HEAD.
    """
    found = {}
    for line in output.splitlines():
        sha, _, name = line.partition("\t")
        if is_commit(sha):
            found[name.strip()] = sha
    if not ref:
        return found.get("HEAD")
    for name in ["refs/tags/{}^{{}}".format(ref), "refs/tags/" + ref, "refs/heads/" + ref, ref]:
        if name in found:
            return found[name]
    return None

def valid_fact(fact, kind):
    """Whether a recorded fact is a well-formed one of its kind.

    Facts come from MODULE.bazel.lock, which is a checked-in file anyone can
    edit or mis-merge; a malformed one is treated as a miss and resolved
    again. A fact of the other kind is malformed too: the two are told apart
    by the key they are stored under, not by their shape.

    Args:
        fact: A value from `module_ctx.facts`, or None.
        kind: `"git"`, for a git module's `?ref=`: exactly `{"commit": ...}`.
            `"registry"`, for a registry module's version: a `location` and
            exactly one of `commit` and `sha256`. Whether that pin suits the
            location is for the caller, who can parse it.

    Returns:
        True if `fact` can be used as one of `kind`.
    """
    if type(fact) != "dict":
        return False
    if kind == "git":
        keys = ["commit"]
    elif kind == "registry" and "commit" in fact:
        keys = ["commit", "location"]
    elif kind == "registry":
        keys = ["location", "sha256"]
    else:
        return False
    if len(fact) != len(keys) or not all([key in fact for key in keys]):
        return False
    for key, value in fact.items():
        if type(value) != "string":
            return False
        if key == "commit" and not is_commit(value):
            return False
        if key == "sha256" and (len(value) != 64 or not all([c in _HEX for c in value.elems()])):
            return False
        if key == "location" and not value:
            return False
    return True

def _parse_version(version):
    parts = []
    for part in version.split("."):
        if not part.isdigit():
            return None
        parts.append(int(part))
    return parts

def check_version(version):
    """Return an error if `version` isn't an exact `<major>.<minor>.<patch>`, else None."""
    parsed = _parse_version(version)
    if parsed == None or len(parsed) != 3:
        return "`{}` is not an exact version such as 1.2.3".format(version)
    return None

def version_gt(a, b):
    """Whether exact version `a` is higher than `b` (both checked by `check_version`)."""
    return _parse_version(a) > _parse_version(b)
