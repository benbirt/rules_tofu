"""What `tf_providers.from_lock` accepts from a `.terraform.lock.hcl`.

The file itself is parsed by `lockjson` (//tf/private/cmd/lockjson, built by
`//tf/private:host_tools.bzl`) with the standard HCL library, and each
entry's address is checked and split there with OpenTofu's registry-address
library, the way `tofu` reads a lock. These pure functions decide what
rules_tofu makes of the entries, so //tf/providers/tests can exercise them
without a module graph. Errors are returned, not raised: the extension
decides which ones abort evaluation and which become a repository that fails
only when fetched, keeping `@tf_providers//:pin` runnable to rewrite the lock.
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
    """Check the registry's digest of a provider zip against the lock.

    The registry says where a platform's zip is and what its SHA256 is; the
    lock's `zh:` hashes are what the user or a bot recorded when they last
    locked. The zip is accepted only if both agree. Lock files list the `zh:`
    hash of every platform the provider publishes, whichever platforms they
    were locked for, so a disagreement means the lock and the registry
    describe different builds.

    Args:
        sha256: The registry's SHA256 of the zip.
        zh: The lock entry's `zh:` hashes.
        context: `<source>@<version> on <platform>`, for the message.

    Returns:
        An error string, or None if the lock vouches for the zip.
    """
    if sha256 in zh:
        return None
    return ("the registry's zip for {} matches none of the lock's `zh:` hashes. The lock " +
            "describes different builds; regenerate it.").format(context)
