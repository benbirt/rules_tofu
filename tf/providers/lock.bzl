"""What `tf_providers.from_lock` accepts from a `.terraform.lock.hcl`.

The file itself is parsed by `lockjson` (//tf/private/cmd/lockjson, built by
`//tf/private:host_tools.bzl`) with the standard HCL library. These pure functions decide
what rules_tofu makes of its entries, so //tf/providers/tests can exercise
them without a module graph. Errors are returned, not raised: the extension
decides which ones abort evaluation and which become a repository that fails
only when fetched (so `@tf_providers//:pin` still runs and can rewrite the
lock).
"""

load(":source.bzl", "DEFAULT_HOST", "is_sha256")

visibility(["//tf/providers/..."])

def lock_entries(providers):
    """Check the provider entries of a lock, as `lockjson` decodes them.

    An entry whose address isn't on the OpenTofu registry is still returned,
    with `error` set, so the extension can name it and point at `pin`.

    Args:
        providers: A list of `{"address", "version", "hashes"}` dicts.

    Returns:
        `struct(entries, error)`. `error` is set if the lock can't be used at
        all. Each entry is `struct(source, namespace, name, version, zh,
        error)`: `source` is the lower-cased `<namespace>/<name>`, `zh` the
        lock's `zh:` hashes (the SHA256s of the provider zips), and `error`
        None if the entry is usable.
    """
    entries = []
    seen = {}
    for provider in providers:
        address = provider.get("address", "")
        parts = address.lower().split("/")
        if len(parts) != 3 or not parts[0] or not parts[1] or not parts[2]:
            return struct(
                entries = None,
                error = "provider address {} is not `<host>/<namespace>/<name>`".format(repr(address)),
            )
        host, namespace, name = parts
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

def lock_sha256s(registry_sha256s, zh, context):
    """Keep the registry's digests that the lock vouches for.

    The registry says where each zip is and what its SHA256 is; the lock's
    `zh:` hashes are what the user (or their bot) recorded when they last
    locked. Each platform is accepted only if both agree. Lock files list
    the `zh:` hash of every platform the provider publishes, whichever
    platforms they were locked for, so any disagreement means the lock and
    the registry describe different builds.

    Args:
        registry_sha256s: `{platform: sha256}` from the registry.
        zh: The lock entry's `zh:` hashes.
        context: `<source>@<version>`, for messages.

    Returns:
        `struct(sha256s, error)`; `error` is None on success.
    """
    locked = {h: True for h in zh}
    mismatched = sorted([p for p, digest in registry_sha256s.items() if digest not in locked])
    if mismatched:
        return struct(
            sha256s = None,
            error = ("the registry's zip for {} on {} matches none of the lock's `zh:` hashes. " +
                     "The lock describes different builds; regenerate it.").format(
                context,
                ", ".join(mismatched),
            ),
        )
    return struct(sha256s = dict(registry_sha256s), error = None)
