"""What `tf_providers.from_lock` accepts from a `.terraform.lock.hcl`.

The file itself is parsed by `lockjson` (//tf/private/cmd/lockjson, built by
`//tf/private:host_tools.bzl`) with the standard HCL library, and each
entry's address is checked and split there with OpenTofu's registry-address
library, the way `tofu` reads a lock. These pure functions decide what
rules_tofu makes of the entries, so //tf/providers/tests can exercise them
without a module graph. Errors are returned, not raised: whole-file errors
abort extension evaluation, while unusable entries become
`@tf_providers//<namespace>/<name>` packages that fail to load when used,
keeping `@tf_providers//:pin` runnable to rewrite the lock.
"""

load(":source.bzl", "DEFAULT_HOST", "is_sha256")

visibility(["//tf/providers/..."])

def lock_entries(providers):
    """Check the provider entries of a lock, as `lockjson` decodes them.

    An entry whose address isn't on the OpenTofu registry is still returned,
    with `error` set, so the extension can name it and point at `pin`.

    Args:
        providers: A list of `{"address", "host", "namespace", "name",
            "version", "hashes"}` dicts. `lockjson` has already checked
            that each address is a full, canonical
            `<host>/<namespace>/<name>`, so the parts are in their
            normalised lower-case form.

    Returns:
        `struct(entries, error)`. `error` is set if the lock can't be used at
        all. Each entry is `struct(source, namespace, name, version, zh,
        error)`, where `source` is `<namespace>/<name>`, `zh` lists the lock's
        `zh:` provider-zip SHA256s and `error` is None if the entry is usable.
    """
    entries = []
    seen = {}
    for provider in providers:
        address = provider["address"]
        host = provider["host"]
        namespace = provider["namespace"]
        name = provider["name"]
        source = "{}/{}".format(namespace, name)
        if source in seen:
            return struct(entries = None, error = "{} is locked more than once".format(repr(address)))
        seen[source] = True

        version = provider.get("version", "")
        if not version:
            return struct(entries = None, error = "{} has no `version`".format(repr(address)))

        zh = []
        for h in provider.get("hashes", []):
            if h.startswith("zh:") and is_sha256(h[3:]):
                zh.append(h[3:])

        error = None
        if host != DEFAULT_HOST:
            error = ("{} is locked against {}, but rules_tofu downloads providers from {}, whose " +
                     "builds have different checksums. Lock it with OpenTofu instead.").format(
                address,
                host,
                DEFAULT_HOST,
            )
        elif not zh:
            error = "{} has no `zh:` hashes, so there is nothing to check its downloads against".format(address)
        entries.append(struct(
            source = source,
            namespace = namespace,
            name = name,
            version = version,
            zh = zh,
            error = error,
        ))
    return struct(entries = entries, error = None)

def lock_vouches(sha256, zh, context):
    """Verify that the lock's `zh:` hashes contain a zip's SHA-256 digest.

    Lock entries list the `zh:` hash of every published platform zip regardless
    of which `-platform` flags wrote the lock, so a digest absent from `zh`
    means the registry and the lock describe different builds.

    Args:
        sha256: SHA-256 digest reported by the registry.
        zh: `zh:` digests from the lock entry.
        context: `<source>@<version> on <platform>` for error messages.

    Returns:
        An error string, or None if `sha256` is in `zh`.
    """
    if sha256 in zh:
        return None
    return "the registry's SHA-256 digest for {} matches none of the lock's `zh:` hashes".format(context)
