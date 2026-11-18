"""Bzlmod module extension that downloads OpenTofu providers for `@tf_providers`.

Pins provider plugins via `.terraform.lock.hcl` or `MODULE.bazel` tags for
`tf_library`, `tf_deploy` and `tf_test` targets to use offline:

    tf_providers = use_extension("@rules_tofu//tf/providers:extensions.bzl", "tf_providers")
    tf_providers.from_lock(lock_file = "//:.terraform.lock.hcl")
    use_repo(tf_providers, "tf_providers")

BUILD files reference each provider as `@tf_providers//<namespace>/<name>`.
"""

# Maintainer notes.
#
# Each provider gets a private repo per platform,
# `tf_providers_<ns>_<name>_<os>_<arch>`, whose `:provider` target carries
# that platform's package as `TfProviderInfo` and is visible only to the
# `@tf_providers` hub. The hub's `@tf_providers//<ns>/<name>` is an alias that
# chooses among them with a `select()` over the hub's own `config_setting`s.
# The rules take `providers` in the exec configuration, so the choice follows
# the platform `tofu` runs on, and Bazel fetches only the repo chosen.
# Sources are lower-cased, as OpenTofu normalises provider addresses.
#
# Each platform's zip comes from the OpenTofu registry's download endpoint,
# which gives its URL and SHA256 (see `download_location` in source.bzl),
# unless a tag sets `url_template`. A pinned platform's zip is checked against
# the pin; when fetched via the registry, the registry's checksum is compared
# with the pin first so a wrong pin fails naming the right value. Without a pin
# the registry is trusted: the extension asks it which of `DEFAULT_PLATFORMS`
# it publishes and their SHA256s, and records them as facts in
# MODULE.bazel.lock, which later evaluations and fresh clones reuse.
#
# Tags for the same `source` resolve to one version: the root module's, else
# the highest (`highest_version`). `sha256` maps for the winning version are
# unioned across modules; conflicting digests or `url_template`s fail. The tofu
# extension resolves differently: the root arbitrates and disagreeing
# dependencies are an error. A provider is a per-configuration artefact a
# library depends on, the same domain as Bzlmod's own MVS, whereas the tofu
# binary is one global tool and which version a build runs is the root's call.
#
# Only the root module's `from_lock` counts. A locked provider gets the same
# kind of repo with the lock's version, for every platform in
# `DEFAULT_PLATFORMS`: the lock's hashes don't say which platforms the
# provider publishes. When fetched, a repo resolves its platform's digest from
# the registry and accepts it only if it matches one of the lock's `zh:` hashes, so locked providers record no facts and a bot's lock
# update is complete without touching MODULE.bazel.lock. A lock entry wins over
# dependencies' tags; one the root also tags is a conflict that fails the
# build until `pin` drops it from the lock. A provider that can't be used,
# such as that one or an unusable entry, gets a hub package that fails to load
# naming the problem, so the extension and `pin` stay runnable. The hub is
# always created, and its `:pin` rewrites the lock with `tofu providers lock`
# against the `required_providers` of every tf_library, tf_deploy and tf_test
# in the workspace and their external deps. A locked provider's target carries
# the lock, which builds copy next to the configuration so `tofu init` checks
# against it.

load("//tf/private:fetched_tree.bzl", "walk")
load("//tf/private:repo_metadata.bzl", "reproducible")
load("//toolchain:versions.bzl", "PLATFORMS")
load(":lock.bzl", "lock_entries", "lock_vouches")
load(
    ":source.bzl",
    "DEFAULT_HOST",
    "DEFAULT_PLATFORMS",
    "check_platforms",
    "check_url_template",
    "download_location",
    "highest_version",
    "missing_platform_error",
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

def _download_json(ctx, url, output, context):
    result = ctx.download(url = url, output = output, allow_fail = True)
    if not result.success:
        fail("Could not query the provider registry for {} at {}.".format(context, url))
    return json.decode(ctx.read(output))

def _registry_sha256s(module_ctx, namespace, name, version, fact_key):
    """Each published default platform's SHA256, from the lockfile's facts or the registry.

    Returns `struct(sha256s, error)`. An unreachable registry fails outright;
    `error` is for answers that say the version can't be used.
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

def _lock_error(lock_path, error):
    """The message for a locked provider that can't be used, pointing at the lock and `pin`."""
    return "{}: {}\nRun `bazel run @tf_providers//:pin` to update the lock.".format(lock_path, error)

def _registry_download(repository_ctx, registry_url, context):
    """Where the registry says one platform's zip is, and its SHA256.

    For a locked provider the lock's `zh:` hashes must vouch for that digest.
    Resolved by the repository rule rather than the extension, so nothing
    about a locked version is recorded in MODULE.bazel.lock: the lock alone
    pins it, and a bot that updates only the lock makes a complete change.
    For a tag, the registry's digest must be the pinned one.

    Returns `struct(url, sha256)`.
    """
    attr = repository_ctx.attr
    response_path = "registry_response.json"
    result = repository_ctx.download(url = registry_url, output = response_path, allow_fail = True)
    if not result.success:
        if attr.zh:
            # Every platform's repository is declared for a locked provider,
            # because the lock's hashes don't say which platforms they are
            # for, so this is also where an unpublished platform shows.
            fail(("Could not get {context} from the provider registry at {url}. Either the " +
                  "registry could not be reached, or it publishes no `{platform}` package of " +
                  "this version, and then builds that run OpenTofu on `{platform}` cannot use " +
                  "it.").format(context = context, url = registry_url, platform = attr.platform))
        fail("Could not query the provider registry for {} at {}. A `url_template` on the tag avoids the registry.".format(
            context,
            registry_url,
        ))
    response = json.decode(repository_ctx.read(response_path))
    repository_ctx.delete(response_path)

    sha256 = attr.sha256
    if attr.zh:
        resolved = registry_shasum(response, context)
        error = resolved.error or lock_vouches(resolved.sha256, attr.zh, context)
        if error:
            fail(_lock_error(attr.lock_path, error))
        sha256 = resolved.sha256
    located = registry_download_url(response, sha256, context)
    if located.error:
        fail(located.error)
    return struct(url = located.url, sha256 = sha256)

# Where a provider's repository unpacks its zip.
_PACKAGE_DIR = "package"

def _files_under(path, prefix):
    """Every file below the directory `path`, as paths relative to it joined onto `prefix`.

    A symlink counts if it leads to a file, which Bazel reads as that file;
    one to a directory or to nothing is left out, since no file can be made
    of it and a walk that followed it could circle for ever.
    """
    return sorted([
        "{}/{}".format(prefix, entry.rel)
        for entry in walk(path)
        if not entry.is_dir and (not entry.is_symlink or (entry.path.exists and not entry.path.is_dir))
    ])

def _tf_provider_download_impl(repository_ctx):
    attr = repository_ctx.attr
    source = attr.source
    version = attr.version
    platform = attr.platform
    parsed = _parse_source_or_fail(source)
    namespace, name = parsed.namespace, parsed.name
    os_name, arch = _split_platform_key(platform)

    location = download_location(namespace, name, version, os_name, arch, attr.url_template)
    if location.error:
        fail("tf_providers.provider `{}`: {}".format(source, location.error))
    url = location.url
    sha256 = attr.sha256
    if location.registry_url:
        resolved = _registry_download(
            repository_ctx,
            location.registry_url,
            "{}@{} on {}".format(source, version, platform),
        )
        url = resolved.url
        sha256 = resolved.sha256

    repository_ctx.download_and_extract(
        url = url,
        sha256 = sha256,
        type = "zip",
        output = _PACKAGE_DIR,
    )

    # The whole zip is vendored, files such as LICENSE and README.md beside
    # the binary: tofu checks a lock's `h1:` hashes against the unpacked
    # package, and finds the binary in it by itself.
    paths = _files_under(repository_ctx.path(_PACKAGE_DIR), _PACKAGE_DIR)
    if not paths:
        fail("The package of {}@{} for {} is empty: {} unpacked to no files.".format(
            source,
            version,
            platform,
            url,
        ))

    # An implementation detail of the hub: BUILD files name the provider as
    # `@tf_providers//<namespace>/<name>`, whose alias is the only thing
    # that may see `:provider`.
    repository_ctx.file("WORKSPACE", "")
    repository_ctx.file(
        "BUILD.bazel",
        content = """\
load("@rules_tofu//tf/private:provider.bzl", "tf_provider")

tf_provider(
    name = "provider",
    address = "{address}",
    version = "{version}",
    platform = "{platform}",
    files = [
{files}
    ],{lock}
    visibility = ["@{hub}//:__subpackages__"],
)
""".format(
            address = "{}/{}/{}".format(DEFAULT_HOST, namespace, name),
            version = version,
            platform = platform,
            files = "\n".join(["        \"{}\",".format(path) for path in paths]),
            lock = "\n    lock = \"{}\",".format(attr.lock) if attr.lock else "",
            hub = _HUB,
        ),
        executable = False,
    )

    # The zip above was checked against a digest from the tag or the lock,
    # so the attributes determine the contents.
    return reproducible(repository_ctx)

_tf_provider_download = repository_rule(
    implementation = _tf_provider_download_impl,
    attrs = {
        "source": attr.string(mandatory = True),
        "version": attr.string(mandatory = True),
        # The one `<os>_<arch>` platform whose package this repository holds.
        "platform": attr.string(mandatory = True),
        # The zip's digest, for a provider pinned by a tag.
        "sha256": attr.string(),
        "url_template": attr.string(),
        # Set for a provider pinned by `from_lock`: the label of the lock's
        # copy in the hub repo, which builds copy into each work tree.
        "lock": attr.string(),
        "lock_path": attr.string(),
        # Also for `from_lock`: the lock entry's `zh:` hashes. The registry's
        # digest is then resolved when the repo is fetched, and must be one
        # of them; `sha256` is left empty.
        "zh": attr.string_list(),
    },
)

def _tf_providers_hub_impl(repository_ctx):
    content = ""
    if repository_ctx.attr.lock:
        lock = repository_ctx.path(repository_ctx.attr.lock)
        repository_ctx.watch(lock)
        if lock.exists:
            content = repository_ctx.read(lock)

    # One `config_setting` per platform, for the `select()`s below: a
    # `select()` key can't combine an OS and a CPU constraint by itself.
    settings = "".join([
        """
config_setting(
    name = "{}",
    constraint_values = [
        "{}",
        "{}",
    ],
)
""".format(platform_key, os_constraint, cpu_constraint)
        for platform_key, os_constraint, cpu_constraint, _ in PLATFORMS
    ])

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
{settings}""".format(
            lock_dir = repository_ctx.attr.lock_dir,
            platforms = repr(DEFAULT_PLATFORMS),
            tagged = repr(repository_ctx.attr.tagged),
            has_lock = repr(bool(repository_ctx.attr.lock)),
            settings = settings,
        ),
        executable = False,
    )

    # `@tf_providers//<namespace>/<name>`: the package's default target. It
    # chooses among the provider's per-platform repositories, and Bazel
    # fetches only the one chosen. `tf_library`, `tf_deploy` and `tf_test`
    # take `providers` in the exec configuration, so the choice follows the
    # platform `tofu` runs on.
    for source, platforms in repository_ctx.attr.providers.items():
        namespace, name = source.split("/")
        repository_ctx.file(
            source + "/BUILD.bazel",
            content = """\
alias(
    name = "{name}",
    actual = select(
        {{
{branches}
        }},
        no_match_error = {no_match_error},
    ),
    visibility = ["//visibility:public"],
)
""".format(
                name = name,
                branches = "\n".join([
                    "            \"//:{}\": \"@{}//:provider\",".format(p, repo_name(namespace, name, p))
                    for p in platforms
                ]),
                no_match_error = repr(missing_platform_error(source, repository_ctx.attr.versions[source], platforms)),
            ),
            executable = False,
        )

    # A provider that can't be used has a package that fails to load, naming
    # the problem, when a build needs it.
    for source, error in repository_ctx.attr.errors.items():
        repository_ctx.file(
            source + "/BUILD.bazel",
            content = "fail({})\n".format(repr("\n" + error)),
            executable = False,
        )

_tf_providers_hub = repository_rule(
    implementation = _tf_providers_hub_impl,
    attrs = {
        # The root module's `from_lock` file, if it uses one.
        "lock": attr.label(),
        "lock_dir": attr.string(),
        # `<namespace>/<name>` -> the `<os>_<arch>` platforms the provider has
        # a repository for.
        "providers": attr.string_list_dict(),
        # `<namespace>/<name>` -> version, for each of `providers`.
        "versions": attr.string_dict(),
        # `<namespace>/<name>` -> why the provider can't be used.
        "errors": attr.string_dict(),
        # `<namespace>/<name>` of the root module's tags, which `pin` leaves
        # out of the lock.
        "tagged": attr.string_list(),
    },
)

_provider_tag = tag_class(
    doc = "Pin a provider in `MODULE.bazel` when specifying custom SHA256 digests or a " +
          "download mirror. Root-tagged providers are omitted from the lock by `pin`; a " +
          "provider both root-tagged and present in the lock fails to fetch until `pin` " +
          "rewrites the lock or the tag is removed.",
    attrs = {
        "source": attr.string(
            mandatory = True,
            doc = "Provider source on `registry.opentofu.org` as `<namespace>/<name>`, e.g. " +
                  "`\"hashicorp/aws\"`. Referenced in BUILD files in lower case as " +
                  "`@tf_providers//<namespace>/<name>`.",
        ),
        "version": attr.string(
            mandatory = True,
            doc = "Exact provider version string, e.g. `\"5.70.0\"`.",
        ),
        "sha256": attr.string_dict(
            doc = "Optional per-platform SHA256 digests of the provider zip, keyed by " +
                  "`<os>_<arch>`: `linux_amd64`, `linux_arm64`, `darwin_amd64`, `darwin_arm64` " +
                  "or `windows_amd64`. List every platform builds run OpenTofu on; each build " +
                  "downloads only the package of its own execution platform. Without `url_template`, a digest that differs " +
                  "from `registry.opentofu.org` fails the fetch and reports the registry's " +
                  "value. When unset, the registry's digests for whichever of `linux_amd64`, " +
                  "`linux_arm64`, `darwin_amd64`, `darwin_arm64` and `windows_amd64` are " +
                  "published are recorded in `MODULE.bazel.lock`.",
        ),
        "url_template": attr.string(
            doc = "Optional mirror URL template for provider zips, using `{namespace}`, " +
                  "`{name}`, `{version}`, `{os}` and `{arch}` placeholders, e.g. " +
                  "`\"https://github.com/PostHog/terraform-provider-posthog/releases/download/v{version}/terraform-provider-{name}_{version}_{os}_{arch}.zip\"`. " +
                  "Requires `sha256` and skips querying `registry.opentofu.org`, though the " +
                  "provider is still addressed under `registry.opentofu.org`.",
        ),
    },
)

_from_lock_tag = tag_class(
    doc = "Pin providers from a `.terraform.lock.hcl` updated by " +
          "`bazel run @tf_providers//:pin`, Renovate or Dependabot. Entries must target " +
          "`registry.opentofu.org`; locks written by `terraform` against " +
          "`registry.terraform.io` are rejected. `pin` collects `required_providers` across " +
          "the workspace's `tf_library`, `tf_deploy` and `tf_test` targets and their " +
          "external `deps`. A fetched module's examples are in no target, so they are " +
          "left out. By default `pin` locks " +
          "the five platforms `from_lock` resolves: `linux_amd64`, `linux_arm64`, " +
          "`darwin_amd64`, `darwin_arm64` and `windows_amd64`. Pass " +
          "`-- -platform=<os>_<arch>` to replace that list when a provider omits one of " +
          "them. Locked providers record nothing in `MODULE.bazel.lock`. Only the root " +
          "module's `from_lock` is read, at most once; `from_lock` in dependencies is " +
          "ignored.",
    attrs = {
        "lock_file": attr.label(
            mandatory = True,
            doc = "Label of a file named `.terraform.lock.hcl` in the main repository, such as " +
                  "`\"//:.terraform.lock.hcl\"` or `\"//infra:.terraform.lock.hcl\"`. Write the " +
                  "label as a string literal rather than building it from a variable. " +
                  "Dependabot's `bazel` updater does not clone the repository. It fetches only " +
                  "the root Bazel files and files named in recognised `MODULE.bazel` attributes " +
                  "such as `lock_file`, together with the `BUILD.bazel` or `BUILD` file beside " +
                  "each, before regenerating `MODULE.bazel.lock`. The lock file need not exist " +
                  "before the first `bazel run @tf_providers//:pin` run. Its directory must be " +
                  "a Bazel package with a `BUILD.bazel` or `BUILD` file, which may be empty.",
        ),
    },
)

# Compiled by //tf/private:host_tools.bzl; see //tf/private/cmd/lockjson.
_LOCK_READER = Label("@rules_tofu_host_tools//:bin/lockjson")

def _decode_lock(module_ctx, path, label):
    """The lock's provider entries, as `lockjson` decodes them."""
    result = module_ctx.execute([module_ctx.path(_LOCK_READER), str(path)])
    if result.return_code != 0:
        fail(("{} could not be read:\n{}\nFix the file, or delete it and run " +
              "`bazel run @tf_providers//:pin` to write a fresh one.").format(label, result.stderr.strip()))
    return json.decode(result.stdout)["providers"]

def _read_lock(module_ctx, mod):
    """Read the root module's `from_lock` tag into `struct(label, lock_path, lock_dir, entries)`, or None."""
    tags = mod.tags.from_lock
    if not tags:
        return None
    if len(tags) > 1:
        fail("tf_providers.from_lock is used more than once; the workspace has one lock file")

    label = tags[0].lock_file
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
        parsed = lock_entries(_decode_lock(module_ctx, path, label))
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
    # then group by normalised source, so `PostHog/posthog` and
    # `posthog/posthog` are the same provider.
    all_tags = []

    # The repos the root is expected to `use_repo`: just the hub. The
    # per-provider repos behind it are private implementation details.
    root_repos = []
    lock = None
    for mod in module_ctx.modules:
        # Only the root module's lock counts, as in OpenTofu, where a child
        # module's lock is ignored: the root decides versions and libraries
        # only constrain them. A dependency's `from_lock`, left over from
        # developing it as its own root module, is ignored rather than an error.
        if mod.is_root:
            root_repos.append(_HUB)
            lock = _read_lock(module_ctx, mod)
        for tag in mod.tags.provider:
            parsed = _parse_source_or_fail(tag.source)
            error = check_platforms(tag.sha256.keys())
            if not error and tag.url_template:
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
            locked[entry.source] = entry
            if entry.source not in by_source:
                by_source[entry.source] = []

    # What the hub is told of each provider: the platforms it has a
    # repository for and its version, or why it can't be used.
    platforms = {}
    versions = {}
    errors = {}

    # Phase 2: Resolve each provider to one version, with a repository for
    # each platform's package.
    for source, tags in by_source.items():
        namespace, name = source.split("/")

        # Validate: at most one root declaration per provider.
        root_tags = [t for t in tags if t.is_root]
        if len(root_tags) > 1:
            fail("Root module declares provider `{}` more than once".format(source))

        entry = locked.get(source)
        if entry:
            # A root tag for a locked provider is a conflict the provider's
            # package in the hub reports when a build needs it, never a
            # `fail()` here: `pin`, which writes the lock without the root's
            # tagged providers, is the way to resolve it and must stay
            # runnable (issue #85).
            error = _lock_provider_repos(lock, entry, conflict = bool(root_tags))
            if error:
                errors[source] = error
            else:
                platforms[source] = DEFAULT_PLATFORMS
                versions[source] = entry.version
            continue

        # Determine the winning version.
        if root_tags:
            # Root module wins unconditionally.
            resolved_version = root_tags[0].version
        else:
            # The one version the tags agree on, else the highest.
            highest = highest_version(source, tags)
            if highest.error:
                # Reported by the provider's package in the hub when a
                # build needs it, not here: a root module that pins with
                # `from_lock` settles this by locking the provider, so `pin`
                # must stay runnable, as for a lock entry that can't be used.
                errors[source] = highest.error
                continue
            resolved_version = highest.version

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

        # The tags that declared the winning version must agree on url_template
        # or leave it unset.
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

        # Phase 3: Create a repository for each platform's package. Bazel
        # fetches one only when a build selects it.
        for platform, digest in merged_sha256.items():
            _tf_provider_download(
                name = repo_name(namespace, name, platform),
                source = source,
                version = resolved_version,
                platform = platform,
                sha256 = digest,
                url_template = url_templates.keys()[0] if url_templates else "",
            )
        platforms[source] = sorted(merged_sha256.keys())
        versions[source] = resolved_version

    # Always created, so that BUILD files in any module can name providers as
    # `@tf_providers//<namespace>/<name>` however the root pins them.
    _tf_providers_hub(
        name = _HUB,
        lock = lock.label if lock else None,
        lock_dir = lock.lock_dir if lock else "",
        providers = platforms,
        versions = versions,
        errors = errors,
        tagged = sorted({t.source: True for t in all_tags if t.is_root}.keys()),
    )

    # `reproducible = True`: every download is gated by a sha256 pinned on a
    # tag, recorded in `facts` or matched against the lock's `zh:` hashes. The
    # repos are therefore a pure function of the module graph, the lock file
    # and `facts`; recording the repo specs in MODULE.bazel.lock would pin
    # nothing extra. Returning only this evaluation's facts drops entries for
    # versions no longer used.
    non_dev = module_ctx.root_module_has_non_dev_dependency
    return module_ctx.extension_metadata(
        root_module_direct_deps = root_repos if non_dev else [],
        root_module_direct_dev_deps = [] if non_dev else root_repos,
        reproducible = True,
        facts = facts,
    )

def _lock_provider_repos(lock, entry, conflict):
    """Create the repos for a provider pinned by the lock, one per platform.

    Every platform in `DEFAULT_PLATFORMS` gets one, since the lock's hashes
    don't say which platforms the provider publishes. Each repo, not the
    extension, asks the registry for its digest and checks it against the
    lock's `zh:` hashes (see `_registry_download`), so a locked provider
    records no facts: the lock is the whole pin.

    A lock entry that can't be used gets no repos. Its error is returned for
    the hub to report when a build needs the provider, rather than failing
    the extension: that would also take down `//:pin`, the way to
    fix the lock. `conflict` is whether the root module also tags the
    provider, which `pin` resolves by leaving it out of the lock; that message
    takes precedence over the entry's own.

    Returns the error, or None.
    """
    error = entry.error
    if conflict:
        error = ("{} is pinned both by a tf_providers.provider tag in the root module's MODULE.bazel " +
                 "and by this lock. Remove the tag, or regenerate the lock, which `pin` writes without " +
                 "the providers the root module tags.").format(entry.source)
    if error:
        return _lock_error(lock.lock_path, error)
    for platform in DEFAULT_PLATFORMS:
        _tf_provider_download(
            name = repo_name(entry.namespace, entry.name, platform),
            source = entry.source,
            version = entry.version,
            platform = platform,
            lock = "@{}//:.terraform.lock.hcl".format(_HUB),
            lock_path = lock.lock_path,
            zh = entry.zh,
        )
    return None

tf_providers = module_extension(
    doc = """Downloads OpenTofu providers and exposes `@tf_providers`.

BUILD files reference providers as `@tf_providers//<namespace>/<name>` in lower
case and without a version. Only providers required by built targets are
fetched, and `tofu init` runs offline against them.

Versions come from `.terraform.lock.hcl` via `from_lock` or from `provider` tags
in `MODULE.bazel`, and the two can be mixed across providers. Archives come from
`registry.opentofu.org` unless a tag sets `url_template`, and every download is
verified against the lock's `zh:` hashes, a tag's `sha256`, or registry digests
recorded in `MODULE.bazel.lock`.

Each provider resolves to one version across the module graph. The root module's
lock or tag wins; otherwise an agreed version or the highest plain numeric
version is selected, comparing dot-separated digit components so `5.70` equals
`5.70.0`. If dependencies request different versions and any is not plain
numeric, such as `2.0.0-rc1` or `v1.2.3`, fetching that provider fails until
the root module selects a version via a tag or `pin`. For the winning version,
`sha256` maps are merged across modules, and conflicting digests or
`url_template` values fail analysis.

Each build downloads only the package for its execution platform, the one
`tofu` runs on: `linux_amd64`, `linux_arm64`, `darwin_amd64`, `darwin_arm64` or
`windows_amd64`.

`from_lock` parses the lock with a Go helper compiled by `rules_go`'s host Go
SDK, honouring `GOPROXY`, `GONOSUMDB`, `GOPRIVATE`, `GONOPROXY` and `GOSUMDB`;
workspaces that register a Go SDK must select a version at least as new as the
`go` directive in `rules_tofu`'s `go.mod`.

Import only `tf_providers` with `use_repo`. See
[Providers](https://github.com/benbirt/rules_tofu#providers).
""",
    implementation = _tf_providers_extension_impl,
    tag_classes = {
        "provider": _provider_tag,
        "from_lock": _from_lock_tag,
    },
)
