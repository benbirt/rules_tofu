"""Bzlmod module extension that downloads pinned OpenTofu/Terraform provider
plugins and exposes them as `tf_provider` build targets, ready to be
referenced from `tf_library(providers = [...])` and
`tf_deploy(providers = [...])`.

Downstream usage in MODULE.bazel:

    tf_providers = use_extension(
        "@rules_tofu//tf/providers:extensions.bzl",
        "tf_providers",
    )
    tf_providers.provider(
        source = "hashicorp/aws",
        version = "5.70.0",
        sha256 = {
            "linux_amd64":   "...",
            "linux_arm64":   "...",
            "darwin_amd64":  "...",
            "darwin_arm64":  "...",
            "windows_amd64": "...",
        },
    )
    use_repo(tf_providers, "tf_providers")

BUILD files name each provider through the `@tf_providers` hub repo as
`@tf_providers//<ns>/<name>`, with `<ns>/<name>` lower-cased as OpenTofu
normalizes provider addresses (e.g. `@tf_providers//posthog/posthog` for
`PostHog/posthog`). Behind each alias is a private per-provider repo,
`tf_providers_<ns>_<name>`, whose `:provider` target only the hub can see.

Each platform's zip comes from the OpenTofu registry (`registry.opentofu.org`),
whose provider download endpoint gives the zip's URL and SHA256: the same
binaries a plain `tofu init` fetches. A tag's `url_template`, e.g.
`"https://example.com/terraform-provider-{name}_{version}_{os}_{arch}.zip"`,
takes precedence, for a mirror or to avoid needing the registry when the
repository is fetched (see `download_location` in `source.bzl`).

`sha256` is optional. A pinned platform's zip is checked against the pin
(and the registry's checksum is compared with it first, so a wrong pin fails
naming the right value). Without a pin, the registry is trusted: the
extension asks it which of `DEFAULT_PLATFORMS` it publishes for that version
and what their SHA256s are, and records them as facts in `MODULE.bazel.lock`.
Later evaluations, and fresh clones with that lockfile, reuse
the recorded digests without asking again, so builds stay reproducible as
long as the lockfile is committed. A `url_template` needs a `sha256`: there
is nothing to ask for the digest.

When multiple modules in the dependency graph request the same provider (same
`source`), the extension resolves to a single version:

  1. If the **root module** specifies a version, that version wins.
  2. Otherwise the **highest version** across all requesting modules wins
     (simple integer-component semver comparison).

For the winning version, `sha256` maps are unioned across all modules that
declared that version. If two modules provide conflicting sha256 values for
the same platform at the same version, or different `url_template`s, the
extension fails with a clear error.

//toolchain:extensions.bzl resolves the OpenTofu version differently — the
root arbitrates and disagreeing dependencies are an error, never
highest-wins. The asymmetry is deliberate: a provider is a
per-configuration artifact a library genuinely depends on, which puts it in
the same domain as Bzlmod's own MVS, where taking the highest version is
the norm. The tofu binary is a single global tool registered once for the
whole build, and which version a build runs is the root module's call.

The hub's `@tf_providers//<ns>/<name>` then carries `TfProviderInfo` and is
passed to `tf_library`/`tf_deploy` via their `providers` attribute. A digest, pinned or recorded, gates every
download, so the provider tree is reproducible and is fetched once per
workspace.

The documented default is for the root module to take its pins from a
`.terraform.lock.hcl` instead:

    tf_providers.from_lock(lock = "//:.terraform.lock.hcl")
    use_repo(tf_providers, "tf_providers")

Only the root module's lock counts. A dependency's `from_lock` is ignored, as
OpenTofu ignores a child module's lock: libraries constrain versions, and the
root decides them.

Each locked provider gets the same `tf_providers_<ns>_<name>` repo, with
the version from the lock and, per platform, the registry's digest, which is
accepted only if it is one of the lock's `zh:` hashes. A lock entry wins
over tags from dependencies, as a root tag does; the root may not also tag it.
The hub repo `@tf_providers`, always created, aliases
`@tf_providers//<ns>/<name>` onto every resolved provider's repo, and `@tf_providers//:pin` rewrites the lock with
`tofu providers lock`, against the `required_providers` of every
`tf_library` and `tf_deploy` in the workspace (a `versions.tf` library
beside the lock pins them). A locked provider's target carries the lock, which
builds copy next to the configuration so `tofu init` checks against it.
A lock entry that can't be used becomes a repo that fails when fetched, so
that `pin` stays runnable.

See `examples/aws/`, `examples/gcp/`, `examples/posthog/` and
`examples/lock/` in-tree for full end-to-end uses.
"""

load(":lock.bzl", "lock_sha256s", "parse_lock")
load(
    ":source.bzl",
    "DEFAULT_HOST",
    "DEFAULT_PLATFORMS",
    "check_url_template",
    "download_location",
    "is_provider_binary",
    "parse_source",
    "published_platforms",
    "registry_download_endpoint",
    "registry_download_url",
    "registry_shasum",
    "registry_versions_endpoint",
    "repo_name",
    "valid_cached_sha256s",
)

# The hub repo, always created: `@tf_providers//<namespace>/<name>` for every
# resolved provider, and `@tf_providers//:pin` for the root module's lock.
_HUB = "tf_providers"

# (os/arch -> exe suffix). Keys must match the `sha256` map's platform keys.
_EXE_SUFFIX = {
    "linux_amd64": "",
    "linux_arm64": "",
    "linux_386": "",
    "darwin_amd64": "",
    "darwin_arm64": "",
    "windows_amd64": ".exe",
    "windows_386": ".exe",
}

def _parse_source_or_fail(source):
    parsed = parse_source(source)
    if parsed.error:
        fail(parsed.error)
    return parsed

def _split_platform_key(platform_key):
    parts = platform_key.split("_", 1)
    if len(parts) != 2 or not parts[0] or not parts[1]:
        fail("provider platform key must be \"<os>_<arch>\", got {}".format(repr(platform_key)))
    return parts[0], parts[1]

def _parse_version(version):
    """Parse a version string into a list of integers for comparison.

    Handles standard semver `major.minor.patch`. Pre-release suffixes are
    not supported and will cause a fail().
    """
    parts = version.split(".")
    result = []
    for part in parts:
        if not part.isdigit():
            fail("Cannot parse version component `{}` in version `{}`; only numeric components are supported".format(part, version))
        result.append(int(part))
    return result

def _version_gt(a, b):
    """Return True if version string `a` is strictly greater than `b`."""
    pa = _parse_version(a)
    pb = _parse_version(b)

    # Pad the shorter list with zeros so [5, 70] and [5, 70, 0] compare equal.
    max_len = max(len(pa), len(pb))
    for _ in range(max_len - len(pa)):
        pa.append(0)
    for _ in range(max_len - len(pb)):
        pb.append(0)

    for i in range(max_len):
        if pa[i] > pb[i]:
            return True
        if pa[i] < pb[i]:
            return False
    return False

def _format_dict(attr_name, entries):
    """Format `entries` as a Starlark dict attribute for the generated BUILD.

    `entries` is a list of `(path, platform_key)` tuples. The dict is keyed
    by file label (so `label_keyed_string_dict` resolves each to a File) and
    valued by platform key string.
    """
    lines = ["    {} = {{".format(attr_name)]
    for binary_path, platform_key in entries:
        lines.append("        \"{}\": \"{}\",".format(binary_path, platform_key))
    lines.append("    },")
    return "\n".join(lines)

def _resolve_via_registry(repository_ctx, registry_url, pinned_sha256, context):
    """Fetch the registry's download metadata and return the zip's URL."""
    response_path = "registry_response.json"
    result = repository_ctx.download(
        url = registry_url,
        output = response_path,
        allow_fail = True,
    )
    if not result.success:
        fail("Could not query the provider registry for {} at {}. A `url_template` on the tag avoids the registry.".format(
            context,
            registry_url,
        ))
    response = json.decode(repository_ctx.read(response_path))
    repository_ctx.delete(response_path)
    resolved = registry_download_url(response, pinned_sha256, context)
    if resolved.error:
        fail(resolved.error)
    return resolved.url

def _download_json(module_ctx, url, output, context):
    result = module_ctx.download(url = url, output = output, allow_fail = True)
    if not result.success:
        fail("Could not query the provider registry for {} at {}.".format(context, url))
    return json.decode(module_ctx.read(output))

def _registry_sha256s(module_ctx, namespace, name, version, fact_key):
    """Each published default platform's SHA256, from the lockfile's facts or the registry.

    Returns `struct(sha256s, error)`. An unreachable registry still fails
    outright; `error` is for answers that say the version can't be used.
    """
    cached = module_ctx.facts.get(fact_key)
    if valid_cached_sha256s(cached):
        return struct(sha256s = dict(cached), error = None)

    context = "{}/{}@{}".format(namespace, name, version)
    published = published_platforms(
        _download_json(
            module_ctx,
            registry_versions_endpoint(namespace, name),
            "registry/{}_{}_versions.json".format(namespace, name),
            context,
        ),
        version,
        context,
    )
    if published.error:
        return struct(sha256s = None, error = published.error)

    sha256s = {}
    for platform_key in published.platforms:
        os_name, arch = _split_platform_key(platform_key)
        platform_context = "{} on {}".format(context, platform_key)
        resolved = registry_shasum(
            _download_json(
                module_ctx,
                registry_download_endpoint(namespace, name, version, os_name, arch),
                "registry/{}_{}_{}_{}.json".format(namespace, name, version, platform_key),
                platform_context,
            ),
            platform_context,
        )
        if resolved.error:
            return struct(sha256s = None, error = resolved.error)
        sha256s[platform_key] = resolved.sha256
    return struct(sha256s = sha256s, error = None)

def _files_under(path, prefix):
    """Every file below the directory `path`, as paths relative to it joined onto `prefix`."""
    files = []
    pending = [(path, prefix)]

    # Starlark has no `while`; one iteration per directory.
    for _ in range(1000):
        if not pending:
            break
        directory, rel = pending.pop()
        for child in directory.readdir():
            child_rel = "{}/{}".format(rel, child.basename)
            if child.is_dir:
                pending.append((child, child_rel))
            else:
                files.append(child_rel)
    if pending:
        fail("{} holds more directories than a provider zip should".format(path))
    return sorted(files)

def _tf_provider_download_impl(repository_ctx):
    source = repository_ctx.attr.source
    version = repository_ctx.attr.version
    sha256 = repository_ctx.attr.sha256
    parsed = _parse_source_or_fail(source)
    namespace, name = parsed.namespace, parsed.name

    # A lock entry the extension couldn't vouch for. Failing here rather than
    # in the extension keeps `@tf_providers//:pin` runnable, so the lock can
    # be regenerated.
    if repository_ctx.attr.error:
        fail("{}: {}\nRun `bazel run @tf_providers//:pin` to update the lock.".format(
            repository_ctx.attr.lock_path,
            repository_ctx.attr.error,
        ))

    if not sha256:
        fail("tf_providers.provider `{}` must declare at least one platform in `sha256`".format(
            repository_ctx.attr.name,
        ))

    address = "{}/{}/{}".format(DEFAULT_HOST, namespace, name)
    entries = []
    package_entries = []
    for platform_key in sorted(sha256.keys()):
        if platform_key not in _EXE_SUFFIX:
            fail("tf_providers.provider `{}` declares unknown platform `{}`. Known: {}".format(
                repository_ctx.attr.name,
                platform_key,
                sorted(_EXE_SUFFIX.keys()),
            ))
        os_name, arch = _split_platform_key(platform_key)
        extract_dir = "plugins/{host}/{ns}/{name}/{version}/{platform}".format(
            host = DEFAULT_HOST,
            ns = namespace,
            name = name,
            version = version,
            platform = platform_key,
        )
        location = download_location(
            namespace,
            name,
            version,
            os_name,
            arch,
            repository_ctx.attr.url_template,
        )
        if location.error:
            fail("tf_providers.provider `{}`: {}".format(source, location.error))
        url = location.url
        if location.registry_url:
            url = _resolve_via_registry(
                repository_ctx,
                location.registry_url,
                sha256[platform_key],
                "{}@{} on {}".format(source, version, platform_key),
            )

        repository_ctx.download_and_extract(
            url = url,
            sha256 = sha256[platform_key],
            type = "zip",
            output = extract_dir,
        )

        # The zip holds the provider binary alongside files such as LICENSE
        # and CHANGELOG.md; see `is_provider_binary` for the names accepted.
        exe_suffix = _EXE_SUFFIX[platform_key]
        binary_filename = None
        for child in repository_ctx.path(extract_dir).readdir():
            basename = child.basename
            if is_provider_binary(basename, name, version, exe_suffix):
                if binary_filename != None:
                    fail(("Ambiguous provider binary for {}@{} on {}: both `{}` and " +
                          "`{}` look like it. Refusing to guess.").format(
                        source,
                        version,
                        platform_key,
                        binary_filename,
                        basename,
                    ))
                binary_filename = basename
        if binary_filename == None:
            fail("No provider binary (`terraform-provider-{}{}` or `terraform-provider-{}_v{}*{}`) found in {} after extracting {}".format(
                name,
                exe_suffix,
                name,
                version,
                exe_suffix,
                extract_dir,
                url,
            ))

        binary_path = "{}/{}".format(extract_dir, binary_filename)
        entries.append((binary_path, platform_key))

        # The rest of the zip (LICENSE, README and the like) is vendored too:
        # tofu checks a lock's `h1:` hashes against the whole unpacked
        # package, not just the binary.
        for path in _files_under(repository_ctx.path(extract_dir), extract_dir):
            if path != binary_path:
                package_entries.append((path, platform_key))

    # Extracted files live in nested directories; Bazel doesn't auto-expose
    # them as source labels, so the `label_keyed_string_dict`s on
    # `tf_provider` couldn't resolve them without an explicit
    # `exports_files()`.
    exports_lines = ["exports_files(["]
    for path, _ in entries + package_entries:
        exports_lines.append("    \"{}\",".format(path))
    exports_lines.append("], visibility = [\"//visibility:private\"])")

    # An implementation detail of the hub: BUILD files name the provider as
    # `@tf_providers//<namespace>/<name>`, whose alias is the only thing
    # that may see `:provider`.
    repository_ctx.file("WORKSPACE", "")
    repository_ctx.file(
        "BUILD.bazel",
        content = """\
load("@rules_tofu//tf/private:provider.bzl", "tf_provider")

{exports}

tf_provider(
    name = "provider",
    address = "{address}",
    version = "{version}",
{binaries}
{package_files}{lock}
    visibility = ["@{hub}//:__subpackages__"],
)
""".format(
            exports = "\n".join(exports_lines),
            address = address,
            version = version,
            binaries = _format_dict("binaries", entries),
            package_files = _format_dict("package_files", package_entries),
            lock = "\n    lock = \"{}\",".format(repository_ctx.attr.lock) if repository_ctx.attr.lock else "",
            hub = _HUB,
        ),
        executable = False,
    )

_tf_provider_download = repository_rule(
    implementation = _tf_provider_download_impl,
    attrs = {
        "source": attr.string(mandatory = True),
        "version": attr.string(mandatory = True),
        "sha256": attr.string_dict(),
        "url_template": attr.string(),
        # Set for a provider pinned by `from_lock`: the label of the lock's
        # copy in the hub repo, which builds copy into each work tree.
        "lock": attr.string(),
        "lock_path": attr.string(),
        "error": attr.string(),
    },
)

def _tf_providers_hub_impl(repository_ctx):
    content = ""
    if repository_ctx.attr.lock:
        lock = repository_ctx.path(repository_ctx.attr.lock)
        repository_ctx.watch(lock)
        if lock.exists:
            content = repository_ctx.read(lock)

    repository_ctx.file("WORKSPACE", "")
    repository_ctx.file(".terraform.lock.hcl", content, executable = False)
    repository_ctx.file(
        "BUILD.bazel",
        content = """\
load("@rules_tofu//tf/private:pin.bzl", "tf_pin")

package(default_visibility = ["//visibility:public"])

exports_files([".terraform.lock.hcl"])

tf_pin(
    name = "pin",
    has_lock = {has_lock},
    lock_dir = "{lock_dir}",
    platforms = {platforms},
    exclude_sources = {tagged},
)
""".format(
            lock_dir = repository_ctx.attr.lock_dir,
            platforms = repr(DEFAULT_PLATFORMS),
            tagged = repr(repository_ctx.attr.tagged),
            has_lock = repr(bool(repository_ctx.attr.lock)),
        ),
        executable = False,
    )

    # `@tf_providers//<namespace>/<name>`: the package's default target.
    for package, repo in repository_ctx.attr.providers.items():
        repository_ctx.file(
            package + "/BUILD.bazel",
            content = """\
alias(
    name = "{name}",
    actual = "@{repo}//:provider",
    visibility = ["//visibility:public"],
)
""".format(name = package.split("/")[1], repo = repo),
            executable = False,
        )

_tf_providers_hub = repository_rule(
    implementation = _tf_providers_hub_impl,
    attrs = {
        # The root module's `from_lock` file, if it uses one.
        "lock": attr.label(),
        "lock_dir": attr.string(),
        # `<namespace>/<name>` -> the repo vendoring that provider.
        "providers": attr.string_dict(),
        # `<namespace>/<name>` of the root module's tags, which `pin` leaves
        # out of the lock.
        "tagged": attr.string_list(),
    },
)

_provider_tag = tag_class(
    attrs = {
        "source": attr.string(
            mandatory = True,
            doc = "Provider source in `<namespace>/<name>` form, e.g. \"hashicorp/aws\".",
        ),
        "version": attr.string(
            mandatory = True,
            doc = "Provider version to pin, e.g. \"5.70.0\".",
        ),
        "sha256": attr.string_dict(
            doc = "Optional per-platform SHA256 of the provider zip. Keys are `<os>_<arch>` " +
                  "(e.g. `linux_amd64`). If set, declare every platform you intend to run on; " +
                  "build/runtime fail at deploy time if the exec platform is missing. If unset, " +
                  "the OpenTofu registry's digests are used for each of " +
                  "linux_amd64, linux_arm64, darwin_amd64, darwin_arm64 and windows_amd64 it " +
                  "publishes, and recorded in MODULE.bazel.lock.",
        ),
        "url_template": attr.string(
            doc = "Optional URL of each platform's provider zip, with `{namespace}`, `{name}`, " +
                  "`{version}`, `{os}` and `{arch}` placeholders, used instead of the OpenTofu " +
                  "registry (registry.opentofu.org). Needs `sha256`.",
        ),
    },
)

_from_lock_tag = tag_class(
    attrs = {
        "lock": attr.label(
            doc = "The workspace's `.terraform.lock.hcl`. Defaults to `//:.terraform.lock.hcl`. " +
                  "The file need not exist yet: `bazel run @tf_providers//:pin` writes it.",
        ),
    },
)

def _read_lock(module_ctx, mod):
    """The root module's `from_lock` tag, read: `struct(label, lock_dir, entries)`, or None."""
    tags = mod.tags.from_lock
    if not tags:
        return None
    if len(tags) > 1:
        fail("tf_providers.from_lock is used more than once; the workspace has one lock file")

    label = tags[0].lock or Label("@@//:.terraform.lock.hcl")
    if label.repo_name != "":
        fail("tf_providers.from_lock: the lock must be in the main repository, got {}".format(label))
    lock_dir, _, basename = (label.package + "/" + label.name).lstrip("/").rpartition("/")
    if basename != ".terraform.lock.hcl":
        fail("tf_providers.from_lock: the lock must be named `.terraform.lock.hcl` (as tofu writes it), got {}".format(label))

    # A missing lock is an empty one, so a new workspace can `pin` its first.
    path = module_ctx.path(label)
    module_ctx.watch(path)
    entries = []
    if path.exists:
        parsed = parse_lock(module_ctx.read(path))
        if parsed.error:
            fail(("{}: {}\nFix the file, or delete it and run `bazel run @tf_providers//:pin` " +
                  "to write a fresh one.").format(label, parsed.error))
        entries = parsed.entries
    return struct(
        label = label,
        lock_path = (lock_dir + "/" if lock_dir else "") + basename,
        lock_dir = lock_dir,
        entries = entries,
    )

def _tf_providers_extension_impl(module_ctx):
    # Phase 1: Collect all provider tags into a flat list of structs,
    # then group by normalized source, so `PostHog/posthog` and
    # `posthog/posthog` are the same provider.
    all_tags = []

    # The repos the root is expected to `use_repo`: just the hub. The
    # per-provider repos behind it are private implementation details.
    root_repos = []
    lock = None
    for mod in module_ctx.modules:
        # Only the root module's lock counts, as in OpenTofu, where a child
        # module's lock is ignored: the root decides versions, and libraries
        # only constrain them. A dependency's `from_lock` (from developing it
        # as a root module of its own) is ignored, not an error.
        if mod.is_root:
            root_repos.append(_HUB)
            lock = _read_lock(module_ctx, mod)
        for tag in mod.tags.provider:
            parsed = _parse_source_or_fail(tag.source)
            if tag.url_template:
                error = check_url_template(tag.url_template)
                if error:
                    fail("tf_providers.provider `{}` in module `{}`: {}".format(tag.source, mod.name, error))
            all_tags.append(struct(
                source = "{}/{}".format(parsed.namespace, parsed.name),
                version = tag.version,
                sha256 = tag.sha256,
                url_template = tag.url_template,
                is_root = mod.is_root,
                mod_name = mod.name,
            ))

    # Digests the extension resolved from the registry, keyed by
    # `<source>@<version>`, to record in MODULE.bazel.lock.
    facts = {}

    by_source = {}
    for t in all_tags:
        if t.source not in by_source:
            by_source[t.source] = []
        by_source[t.source].append(t)

    # Providers pinned by the lock. Like a root tag, a lock entry wins over
    # whatever dependencies ask for.
    locked = {}
    if lock:
        for entry in lock.entries:
            if [t for t in by_source.get(entry.source, []) if t.is_root]:
                fail(("Provider `{}` is pinned both by a tf_providers.provider tag and by {}. " +
                      "Declare it one way.").format(entry.source, lock.label))
            locked[entry.source] = entry
            if entry.source not in by_source:
                by_source[entry.source] = []

    # Phase 2: Resolve each provider to a single (version, sha256) pair.
    for source, tags in by_source.items():
        namespace, name = source.split("/")
        entry = locked.get(source)
        if entry:
            _lock_provider_repo(module_ctx, lock, entry, facts)
            continue

        # Validate: at most one root declaration per provider.
        root_tags = [t for t in tags if t.is_root]
        if len(root_tags) > 1:
            fail("Root module declares provider `{}` more than once".format(source))

        # Determine the winning version.
        if root_tags:
            # Root module wins unconditionally.
            resolved_version = root_tags[0].version
        else:
            # Pick the highest version across all tags.
            resolved_version = tags[0].version
            for t in tags[1:]:
                if _version_gt(t.version, resolved_version):
                    resolved_version = t.version

        # Merge sha256 maps from all tags that declared the winning version.
        merged_sha256 = {}
        for t in tags:
            if t.version != resolved_version:
                continue
            for platform, digest in t.sha256.items():
                if platform in merged_sha256 and merged_sha256[platform] != digest:
                    fail(
                        ("Provider `{}` version `{}`: modules disagree on sha256 for " +
                         "platform `{}` (got `{}` and `{}`)").format(
                            source,
                            resolved_version,
                            platform,
                            merged_sha256[platform],
                            digest,
                        ),
                    )
                merged_sha256[platform] = digest

        # The tags that declared the winning version must agree on where the
        # zips come from (or leave it to the default).
        url_templates = {
            t.url_template: t.mod_name
            for t in tags
            if t.version == resolved_version and t.url_template
        }
        if len(url_templates) > 1:
            fail("Provider `{}` version `{}`: modules disagree on url_template ({})".format(
                source,
                resolved_version,
                ", ".join(["`{}` from `{}`".format(u, m) for u, m in url_templates.items()]),
            ))

        # No module pinned the winning version: trust the registry's digests,
        # recorded as facts so they're asked for once per lockfile.
        if not merged_sha256:
            if url_templates:
                fail(("Provider `{}` version `{}` has a url_template but no sha256: pin the " +
                      "sha256 of each platform's zip, since there's no registry to ask for them.").format(
                    source,
                    resolved_version,
                ))
            fact_key = "{}@{}".format(source, resolved_version)
            resolved = _registry_sha256s(module_ctx, namespace, name, resolved_version, fact_key)
            if resolved.error:
                fail(resolved.error)
            merged_sha256 = resolved.sha256s
            facts[fact_key] = merged_sha256

        # Phase 3: Create the repository.
        _tf_provider_download(
            name = repo_name(namespace, name),
            source = source,
            version = resolved_version,
            sha256 = merged_sha256,
            url_template = url_templates.keys()[0] if url_templates else "",
        )

    # Always created, so that BUILD files in any module can name providers as
    # `@tf_providers//<namespace>/<name>` however the root pins them.
    _tf_providers_hub(
        name = _HUB,
        lock = lock.label if lock else None,
        lock_dir = lock.lock_dir if lock else "",
        providers = {source: repo_name(*source.split("/")) for source in by_source},
        tagged = sorted({t.source: True for t in all_tags if t.is_root}.keys()),
    )

    # `reproducible = True`: every download is gated by a sha256, either pinned
    # on a tag or recorded in `facts`, so the repos are a pure function of the
    # module graph, the lock file and the facts, and a lockfile copy of the
    # repo specs would pin nothing they don't. Returning only this
    # evaluation's facts drops ones for versions no longer used.
    non_dev = module_ctx.root_module_has_non_dev_dependency
    return module_ctx.extension_metadata(
        root_module_direct_deps = root_repos if non_dev else [],
        root_module_direct_dev_deps = [] if non_dev else root_repos,
        reproducible = True,
        facts = facts,
    )

def _lock_provider_repo(module_ctx, lock, entry, facts):
    """Create the repo for a provider pinned by the lock.

    The registry's digests are recorded as facts exactly as for an unpinned
    tag; on top of that, each must be one of the lock's `zh:` hashes. A lock
    entry that can't be used yields a repo that fails when fetched, naming
    the problem, rather than failing the extension: that would also take
    down `@tf_providers//:pin`, the way to fix the lock.
    """
    context = "{}@{}".format(entry.source, entry.version)
    error = entry.error
    sha256s = {}
    if not error:
        resolved = _registry_sha256s(module_ctx, entry.namespace, entry.name, entry.version, context)
        error = resolved.error
        if not error:
            facts[context] = resolved.sha256s
            vouched = lock_sha256s(resolved.sha256s, entry.zh, context)
            error = vouched.error
            sha256s = vouched.sha256s or {}

    _tf_provider_download(
        name = repo_name(entry.namespace, entry.name),
        source = entry.source,
        version = entry.version,
        sha256 = sha256s,
        lock = "@{}//:.terraform.lock.hcl".format(_HUB),
        lock_path = lock.lock_path,
        error = error or "",
    )

tf_providers = module_extension(
    implementation = _tf_providers_extension_impl,
    tag_classes = {
        "provider": _provider_tag,
        "from_lock": _from_lock_tag,
    },
)
