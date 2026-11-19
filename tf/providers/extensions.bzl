"""Bzlmod module extension that downloads OpenTofu providers for `@tf_providers`.

Pins provider plugins for `tf_library`, `tf_deploy` and `tf_test` targets to
use offline. Providers on `registry.opentofu.org` are pinned via
`.terraform.lock.hcl`; off-registry providers use `tf_providers.provider`:

    tf_providers = use_extension("@rules_tofu//tf/providers:extensions.bzl", "tf_providers")
    tf_providers.from_lock(lock_file = "//:.terraform.lock.hcl")
    use_repo(tf_providers, "tf_providers")

BUILD files reference each provider as `@tf_providers//<namespace>/<name>`.
"""

# Maintainer notes.
#
# Each provider gets one private repo per platform,
# `tf_providers_<ns>_<name>_<os>_<arch>`, whose `:provider` target carries that
# platform's `TfProviderInfo` and is visible only to `@tf_providers`.
# `@tf_providers//<ns>/<name>` is an alias with a `select()` over
# `config_setting`s in the hub. Rules take `providers` with `cfg = "exec"`, so
# the alias selects for the execution platform where `tofu` runs, and Bazel
# fetches only that platform's repo.
# Sources are lower-cased, as OpenTofu normalises provider addresses.
#
# Only the root module pins providers, matching OpenTofu, where the root lock
# chooses versions and child modules only constrain them. `from_lock` and
# `provider` tags in dependencies are ignored. The extension makes no network
# requests and records nothing in MODULE.bazel.lock.
#
# Providers on registry.opentofu.org are pinned by the lock. Because lock
# hashes carry no platform names, each locked provider gets a repo for every
# platform in `DEFAULT_PLATFORMS`. When fetched, that repo queries the
# registry's download endpoint for its zip URL and SHA-256 and requires the
# digest to match one of the lock's `zh:` hashes. Lock-only updates therefore
# leave MODULE.bazel.lock valid. Locked targets also carry the lock file so
# builds copy it next to the root configuration for `tofu init`.
#
# Off-registry providers are pinned by root `provider` tags setting `source`,
# `version`, `url_template` and `sha256`, with one repo per `sha256` key. `pin`
# excludes root-tagged providers from the lock.
#
# An unusable provider (a lock/tag conflict, an unusable lock entry, or a
# provider tagged only by dependencies) gets a `@tf_providers//<ns>/<name>`
# package whose BUILD file calls `fail()`. The extension itself still succeeds,
# keeping `@tf_providers//:pin` runnable. The hub is always created; `:pin`
# runs `tofu providers lock` across `required_providers` of the workspace's
# tf_library, tf_deploy and tf_test targets and their external deps.

load("//tf/private:fetched_tree.bzl", "walk")
load("//tf/private:repo_metadata.bzl", "reproducible")
load("//toolchain:versions.bzl", "PLATFORMS")
load(":lock.bzl", "lock_entries", "lock_vouches")
load(
    ":source.bzl",
    "DEFAULT_HOST",
    "DEFAULT_PLATFORMS",
    "check_tag",
    "download_location",
    "ignored_tag_error",
    "missing_platform_error",
    "parse_source",
    "registry_download_url",
    "registry_shasum",
    "repo_name",
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

def _lock_error(lock_path, error):
    """Format a lock error for `lock_path` with instructions to run `pin`."""
    return "{}: {}\nRun `bazel run @tf_providers//:pin` to update the lock.".format(lock_path, error)

def _locked_download(repository_ctx, registry_url, context):
    """Resolve a locked provider platform's zip URL and SHA-256 from the registry.

    The digest must match one of the lock's `zh:` hashes. Resolving in the
    repository rule rather than the extension keeps provider digests out of
    MODULE.bazel.lock, so lock-only updates leave MODULE.bazel.lock valid.

    Returns `struct(url, sha256)`.
    """
    attr = repository_ctx.attr
    response_path = "registry_response.json"
    result = repository_ctx.download(url = registry_url, output = response_path, allow_fail = True)
    if not result.success:
        # Locked providers declare a repository for every platform because lock
        # hashes carry no platform names; an unpublished platform fails here
        # when fetched.
        fail(("Failed to query {context} from {url}. Either the registry is unreachable, " +
              "or it publishes no `{platform}` package for this version.").format(
            context = context,
            url = registry_url,
            platform = attr.platform,
        ))
    response = json.decode(repository_ctx.read(response_path))
    repository_ctx.delete(response_path)

    resolved = registry_shasum(response, context)
    error = resolved.error or lock_vouches(resolved.sha256, attr.zh, context)
    if not error:
        located = registry_download_url(response, context)
        error = located.error
    if error:
        fail(_lock_error(attr.lock_path, error))
    return struct(url = located.url, sha256 = resolved.sha256)

# Subdirectory where the provider repository extracts the zip.
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
        resolved = _locked_download(
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

    # Vendor every extracted file, including LICENSE and README.md: `tofu`
    # checks lock `h1:` hashes over the whole directory and locates the
    # provider binary itself.
    paths = _files_under(repository_ctx.path(_PACKAGE_DIR), _PACKAGE_DIR)
    if not paths:
        fail("Provider archive for `{}@{}` on `{}` unpacked no files ({}).".format(
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

    # The zip was verified against the tag's sha256 or the lock's zh: hashes,
    # so the attributes determine the contents.
    return reproducible(repository_ctx)

_tf_provider_download = repository_rule(
    implementation = _tf_provider_download_impl,
    attrs = {
        "source": attr.string(mandatory = True),
        "version": attr.string(mandatory = True),
        # `<os>_<arch>` platform key for this repository's package.
        "platform": attr.string(mandatory = True),
        # Set for a tagged provider: zip URL template and SHA-256 digest.
        "url_template": attr.string(),
        "sha256": attr.string(),
        # Set for a provider pinned by `from_lock`: the label of the lock's
        # copy in the hub repo, which builds copy into each work tree.
        "lock": attr.string(),
        "lock_path": attr.string(),
        # Also for `from_lock`: the lock entry's `zh:` hashes. The registry's
        # digest is resolved on fetch and checked against these; `sha256` is
        # unused.
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

    # One `config_setting` per platform for the hub aliases: a `select()` key
    # cannot combine OS and CPU constraints directly.
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

    # `@tf_providers//<namespace>/<name>` selects among the provider's
    # per-platform repositories; Bazel fetches only the chosen branch. Rules
    # take `providers` with `cfg = "exec"`, selecting for the execution
    # platform where `tofu` runs.
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

    # Unusable providers get a hub package whose BUILD file fails to load when
    # a target depends on it.
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
        # `<namespace>/<name>` -> `<os>_<arch>` platforms with declared
        # download repositories.
        "providers": attr.string_list_dict(),
        # `<namespace>/<name>` -> version for each entry in `providers`.
        "versions": attr.string_dict(),
        # `<namespace>/<name>` -> load-time error message for unusable
        # providers.
        "errors": attr.string_dict(),
        # `<namespace>/<name>` of the root module's tags, which `pin` leaves
        # out of the lock.
        "tagged": attr.string_list(),
    },
)

_provider_tag = tag_class(
    doc = "Pin a provider outside `registry.opentofu.org` (such as a fork, an in-house " +
          "provider or a vendor build published elsewhere) by URL template and " +
          "per-platform SHA-256 digests. Providers on `registry.opentofu.org` are pinned " +
          "with `from_lock` instead. Only root-module tags are used; tags in dependencies " +
          "are ignored. `pin` excludes tagged providers from the lock; a provider " +
          "present in both a root tag and the lock fails when used until the tag is " +
          "removed or `pin` rewrites the lock.",
    attrs = {
        "source": attr.string(
            mandatory = True,
            doc = "Provider source as `<namespace>/<name>`, e.g. `\"acme/widgets\"`, matching " +
                  "`required_providers`. OpenTofu still addresses the provider as " +
                  "`registry.opentofu.org/<namespace>/<name>`. Referenced in BUILD files in " +
                  "lower case as `@tf_providers//<namespace>/<name>`.",
        ),
        "version": attr.string(
            mandatory = True,
            doc = "Exact provider version string, e.g. `\"1.4.0\"`.",
        ),
        # `check_tag` requires `url_template` and `sha256` on root tags.
        # Leaving `mandatory` unset here lets ignored 0.5 tags in dependencies
        # load without failing Bazel's tag check.
        "url_template": attr.string(
            doc = "Required. Download URL template for each platform's zip, using `{namespace}`, " +
                  "`{name}`, `{version}`, `{os}` and `{arch}` placeholders, e.g. " +
                  "`\"https://github.com/acme/terraform-provider-widgets/releases/download/v{version}/terraform-provider-{name}_{version}_{os}_{arch}.zip\"`.",
        ),
        "sha256": attr.string_dict(
            doc = "Required. Map of `<os>_<arch>` platform key (`linux_amd64`, `linux_arm64`, " +
                  "`darwin_amd64`, `darwin_arm64`, `windows_amd64`) to each zip's SHA-256 digest. " +
                  "Building on an unlisted execution platform fails analysis naming the listed " +
                  "platforms.",
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
          "them. Providers record nothing in `MODULE.bazel.lock`. Only the root " +
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
    # The repos the root is expected to `use_repo`: just the hub. The
    # per-platform provider repos are private to the hub.
    root_repos = []
    lock = None

    # Root tags keyed by lower-case `<namespace>/<name>`, and dependency
    # module names keyed by each provider source they tag.
    tags = {}
    ignored = {}
    for mod in module_ctx.modules:
        # Only the root module pins providers, as in OpenTofu, where a child
        # module's lock is ignored: the root decides versions and libraries
        # only constrain them. `from_lock` and `provider` tags in dependencies
        # come from developing them as root modules and are ignored.
        if mod.is_root:
            root_repos.append(_HUB)
            lock = _read_lock(module_ctx, mod)
        for tag in mod.tags.provider:
            parsed = _parse_source_or_fail(tag.source)
            source = "{}/{}".format(parsed.namespace, parsed.name)
            if not mod.is_root:
                ignored.setdefault(source, []).append(mod.name)
                continue
            error = check_tag(tag.sha256, tag.url_template)
            if error:
                fail("tf_providers.provider `{}`: {}.".format(tag.source, error))
            if source in tags:
                fail("Root module declares provider `{}` more than once".format(source))
            tags[source] = tag

    # Hub inputs per provider: declared platforms and version, or a load-time
    # error.
    platforms = {}
    versions = {}
    errors = {}

    # Providers pinned by the lock.
    for entry in (lock.entries if lock else []):
        # Report a lock/tag conflict from the provider's hub package when used,
        # never via `fail()` here, so `@tf_providers//:pin` stays runnable to
        # rewrite the lock without root-tagged providers (issue #85).
        error = _lock_provider_repos(lock, entry, conflict = entry.source in tags)
        if error:
            errors[entry.source] = error
        else:
            platforms[entry.source] = DEFAULT_PLATFORMS
            versions[entry.source] = entry.version

    # Root-tagged providers get one repository per `sha256` key; Bazel fetches
    # only the platform a build selects.
    for source, tag in tags.items():
        if source in errors or source in platforms:
            continue
        namespace, name = source.split("/")
        for platform, digest in tag.sha256.items():
            _tf_provider_download(
                name = repo_name(namespace, name, platform),
                source = source,
                version = tag.version,
                platform = platform,
                url_template = tag.url_template,
                sha256 = digest,
            )
        platforms[source] = sorted(tag.sha256.keys())
        versions[source] = tag.version

    # Providers tagged only by dependencies get a hub package explaining that
    # only the root module pins providers.
    for source, modules in ignored.items():
        if source not in errors and source not in platforms:
            errors[source] = ignored_tag_error(source, modules)

    # Always created, so that BUILD files in any module can name providers as
    # `@tf_providers//<namespace>/<name>` however the root pins them.
    _tf_providers_hub(
        name = _HUB,
        lock = lock.label if lock else None,
        lock_dir = lock.lock_dir if lock else "",
        providers = platforms,
        versions = versions,
        errors = errors,
        tagged = sorted(tags.keys()),
    )

    # `reproducible = True`: every download is gated by a sha256 pinned on a
    # tag or vouched for by the lock's `zh:` hashes. Repo specs are a pure
    # function of root tags and the lock file, so MODULE.bazel.lock needs no
    # copy of them.
    non_dev = module_ctx.root_module_has_non_dev_dependency
    return module_ctx.extension_metadata(
        root_module_direct_deps = root_repos if non_dev else [],
        root_module_direct_dev_deps = [] if non_dev else root_repos,
        reproducible = True,
    )

def _lock_provider_repos(lock, entry, conflict):
    """Declare per-platform download repos for one lock entry, or return an error.

    Declares a repo for every platform in `DEFAULT_PLATFORMS` because lock
    hashes carry no platform names. Each repo resolves its digest from the
    registry and checks it against `entry.zh` on fetch (`_locked_download`),
    keeping provider digests out of MODULE.bazel.lock.

    An unusable entry or a lock/tag conflict (`conflict` is True) declares no
    repos and returns a hub package error instead of failing the extension, so
    `//:pin` stays runnable. When `conflict` is True, the conflict message
    replaces `entry.error`; the provider is unusable until the tag is removed
    or `pin` rewrites the lock.

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
case and without a version. `tofu init` runs offline against them.

Only the root module pins providers. Providers on `registry.opentofu.org` are
pinned by `.terraform.lock.hcl` via `from_lock`. Off-registry providers are
pinned by `provider` tags setting `source`, `version`, `url_template` and
`sha256`. `from_lock` and `provider` tags in dependencies are ignored, so the
root module locks or tags every provider its dependencies use. Nothing about
providers is recorded in `MODULE.bazel.lock`.

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
