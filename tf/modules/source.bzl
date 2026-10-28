"""How `tf_modules.module` reads a module's `source`, and where it is fetched from.

Split out of `extensions.bzl` so these decisions are pure functions that
//tf/modules/tests can exercise without evaluating a module graph or touching
the network. Problems are reported as an `error` string on the returned
struct rather than raised with `fail()`: only the extension and its
repository rule are entitled to abort the build.

Sources take the forms tofu accepts, as //tf/private/cmd/internal/tfconfig
reads them when it matches module calls:

- a registry address, `[<host>/]<namespace>/<name>/<system>`;
- git, `git::<url>[//<subdir>][?ref=<ref>]`, or the shorthand
  `github.com/<owner>/<repo>[//<subdir>][?ref=<ref>]`;
- an archive, `https://<url>[//<subdir>]` (or `file://`), whose type is
  given by its extension or `?archive=<type>`.
"""

visibility(["//tf/modules/..."])

DEFAULT_HOST = "registry.opentofu.org"

_HEX = "0123456789abcdef"

# Archive types tofu (through go-getter) recognises, as Bazel names them.
_ARCHIVE_TYPES = {
    "tar": "tar",
    "tar.bz2": "tar.bz2",
    "tar.gz": "tar.gz",
    "tar.xz": "tar.xz",
    "tbz2": "tar.bz2",
    "tgz": "tar.gz",
    "txz": "tar.xz",
    "zip": "zip",
}

def split_subdir(source):
    """Split off a source's `//<subdir>`, as tofu does.

    The subdir starts at the first `//` that isn't part of a `scheme://`; a
    query string (`?ref=`) stays with the base address.

    Returns:
        `(base, subdir)`, with `subdir` "" when there is none.
    """
    offset = 0
    rest = source
    i = rest.find("://")
    if i >= 0:
        offset = i + 3
        rest = rest[offset:]
    i = rest.find("//")
    if i < 0:
        return source, ""
    base = source[:offset + i]
    subdir = rest[i + 2:]
    q = subdir.find("?")
    if q >= 0:
        base += subdir[q:]
        subdir = subdir[:q]
    return base, subdir.strip("/")

def _registry_parts(source):
    if source.startswith("./") or source.startswith("../") or "::" in source or "://" in source:
        return None
    base, _ = split_subdir(source)
    if "?" in base:
        return None
    parts = base.split("/")
    if len(parts) == 3:
        if "." in parts[0]:
            return None
        parts = [DEFAULT_HOST] + parts
    elif len(parts) == 4:
        if "." not in parts[0] or parts[0].lower() in ("github.com", "bitbucket.org"):
            return None
    else:
        return None
    for p in parts:
        if not p:
            return None
    return parts

def _query(query):
    """Parse `a=b&c=d` into a dict, or return None if malformed."""
    params = {}
    if not query:
        return params
    for pair in query.split("&"):
        key, eq, value = pair.partition("=")
        if not eq or not key:
            return None
        params[key] = value
    return params

def _error(message):
    return struct(kind = None, error = message)

def parse_source(source):
    """Classify and split a `tf_modules.module` tag's `source`.

    Returns:
        `struct(kind, error, ...)`. `kind` is "registry" (with `host`,
        `namespace`, `name`, `system`), "git" (with `remote` and `ref`) or
        "archive" (with `url` and `type`); all but registry addresses also
        have `subdir`. `error` is None on success.
    """
    parts = _registry_parts(source)
    if parts:
        if split_subdir(source)[1]:
            return _error(("tf_modules.module `{}`: declare the module itself, without `//<subdir>`; " +
                           "calls reach its submodules with `//<subdir>` anyway").format(source))
        return struct(
            kind = "registry",
            error = None,
            host = parts[0].lower(),
            namespace = parts[1].lower(),
            name = parts[2].lower(),
            system = parts[3].lower(),
        )

    base, subdir = split_subdir(source)
    address, _, query = base.partition("?")
    params = _query(query)
    if params == None:
        return _error("tf_modules.module `{}`: malformed query string".format(source))

    if address.startswith("git::"):
        remote = address[len("git::"):]
    elif address.startswith("github.com/"):
        repo = address[len("github.com/"):].removesuffix(".git")
        if repo.count("/") != 1:
            return _error("tf_modules.module `{}`: expected github.com/<owner>/<repo>".format(source))
        remote = "https://github.com/{}.git".format(repo)
    else:
        remote = None

    if remote != None:
        unknown = [k for k in params if k not in ("ref", "depth")]
        if unknown:
            return _error("tf_modules.module `{}`: unsupported git option `{}`".format(source, unknown[0]))
        if not remote:
            return _error("tf_modules.module `{}`: no repository URL after `git::`".format(source))
        return struct(kind = "git", error = None, remote = remote, ref = params.get("ref", ""), subdir = subdir)

    if address.startswith("https://") or address.startswith("http://") or address.startswith("file://"):
        archive = params.pop("archive", None)
        if archive == None:
            path = address.rpartition("/")[2].lower()
            for ext in sorted(_ARCHIVE_TYPES, key = len, reverse = True):
                if path.endswith("." + ext):
                    archive = ext
                    break
        if archive not in _ARCHIVE_TYPES:
            return _error(("tf_modules.module `{}`: not an archive tofu would recognise; name one ending in " +
                           "{}, or add `?archive=<type>`").format(source, ", ".join(["." + t for t in sorted(_ARCHIVE_TYPES)])))
        url = address
        if params:
            url += "?" + "&".join(["{}={}".format(k, v) for k, v in params.items()])
        return struct(kind = "archive", error = None, url = url, type = _ARCHIVE_TYPES[archive], subdir = subdir)

    return _error(("tf_modules.module `{}`: unsupported source; use a registry address, `git::<url>`, " +
                   "`github.com/<owner>/<repo>` or the URL of an archive").format(source))

def hub_path(parsed):
    """A registry module's package in `@tf_modules`.

    `<namespace>/<name>/<system>`, prefixed with the host unless it is the
    default one.
    """
    path = "{}/{}/{}".format(parsed.namespace, parsed.name, parsed.system)
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

def valid_fact(fact):
    """Whether a recorded fact is a well-formed `{"commit": ...}` or `{"sha256": ...}`.

    Facts come from MODULE.bazel.lock, which is a checked-in file anyone can
    edit; a malformed one is treated as a miss and resolved again.
    """
    if type(fact) != "dict" or len(fact) == 0:
        return False
    for key, value in fact.items():
        if key == "commit":
            if type(value) != "string" or not is_commit(value):
                return False
        elif key == "sha256":
            if type(value) != "string" or len(value) != 64 or not all([c in _HEX for c in value.elems()]):
                return False
        elif key == "location":
            if type(value) != "string" or not value:
                return False
        else:
            return False
    return "commit" in fact or "sha256" in fact

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
