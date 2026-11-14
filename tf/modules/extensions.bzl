"""Bzlmod module extension that fetches OpenTofu modules for `@tf_modules`.

Exposes external modules from registries, git repositories and archives as
`tf_library` targets so `tofu init` never fetches modules:

    tf_modules = use_extension("@rules_tofu//tf/modules:extensions.bzl", "tf_modules")
    tf_modules.module(source = "terraform-aws-modules/vpc/aws", version = "5.8.1")
    tf_modules.module(
        name = "acme_vpc",
        source = "git::https://github.com/acme/infra.git//vpc?ref=v1.2.0",
    )
    tf_modules.module(
        name = "vpc_archive",
        source = "https://example.com/vpc-1.2.0.tar.gz",
        sha256 = "...",
    )
    use_repo(tf_modules, "tf_modules")

BUILD files then reference each module in `deps`, for example
`@tf_modules//terraform-aws-modules/vpc/aws` or `@tf_modules//acme_vpc`.
"""

load("//tf/private:fetched_tree.bzl", "walk", "within")
load("//tf/private:repo_metadata.bzl", "reproducible")
load(
    ":source.bzl",
    "check_name",
    "check_version",
    "hub_path",
    "is_commit",
    "looks_abbreviated",
    "ls_remote_commit",
    "modules_service_url",
    "registry_download_endpoint",
    "registry_location",
    "repo_name",
    "valid_fact",
    "version_gt",
)

_HUB = "tf_modules"

# The `@tf_providers` hub of //tf/providers:extensions.bzl, which rules_tofu
# imports so that the libraries generated here can name providers.
_PROVIDERS_HUB = "tf_providers"

# Compiled by //tf/private:host_tools.bzl; see //tf/private/cmd/modscan.
_MODSCAN = Label("@rules_tofu_host_tools//:bin/modscan")

def _parse_sources(module_ctx, sources):
    """Read module source addresses as OpenTofu does, with `modscan parse`.

    Returns a `struct(kind, error, host, namespace, name, system, remote, ref,
    url, type, subdir)` for each source: see tfconfig.Source.
    """
    result = module_ctx.execute([module_ctx.path(_MODSCAN), "parse"] + sources)
    if result.return_code != 0:
        fail("Could not read module sources with modscan:\n{}".format(result.stderr.strip()))
    return [
        struct(
            kind = d.get("kind") or None,
            error = d.get("error") or None,
            host = d.get("host", ""),
            namespace = d.get("namespace", ""),
            name = d.get("name", ""),
            system = d.get("system", ""),
            remote = d.get("remote", ""),
            ref = d.get("ref", ""),
            url = d.get("url", ""),
            type = d.get("type", ""),
            subdir = d.get("subdir", ""),
        )
        for d in json.decode(result.stdout)
    ]

# Bazel's own files in a fetched module would split it into packages of its
# own. A .bazelignore, which Bazel reads at the root of any repository, would
# also hide directories from the module's `glob`. They're removed before the
# module's BUILD file is written.
_BAZEL_FILES = [".bazelignore", "BUILD", "BUILD.bazel", "MODULE.bazel", "REPO.bazel", "WORKSPACE", "WORKSPACE.bazel"]

# The generated targets: the module's library, in its directory, and, when
# that is a subdirectory, the rest of the fetched files, which local paths
# may lead out into. Named so that no file of the module can share the name:
# a file `files` beside a `filegroup(name = "files")` fails the package.
_MODULE_TARGET = "_rules_tofu_module"
_FILES_TARGET = "_rules_tofu_files"

# Files excluded from the module's globs: VCS metadata, any committed
# .terraform directory or .terraform.lock.hcl, the generated root BUILD.bazel
# and auto-loaded variable files, which a module's root or examples often
# carry and tf_library rejects in data. tofu reads none of them from a called
# module.
_EXCLUDED = [
    "**/.git/**",
    "**/.terraform/**",
    "**/.terraform.lock.hcl",
    "**/terraform.tfvars",
    "**/terraform.tfvars.json",
    "**/*.auto.tfvars",
    "**/*.auto.tfvars.json",
    "BUILD.bazel",
]

def _git(ctx, args, context, working_directory = ""):
    result = ctx.execute(
        ["git"] + args,
        environment = {"GIT_TERMINAL_PROMPT": "0"},
        working_directory = working_directory,
        timeout = 600,
    )
    if result.return_code != 0:
        fail("{}: `git {}` failed:\n{}".format(context, " ".join(args), result.stderr.strip()))
    return result.stdout

def _resolve_ref(module_ctx, remote, ref, context):
    """The commit a git ref names now, asking the remote unless `ref` is one."""
    if is_commit(ref):
        return ref
    patterns = [ref, ref + "^{}"] if ref else ["HEAD"]
    commit = ls_remote_commit(_git(module_ctx, ["ls-remote", remote] + patterns, context), ref)
    if not commit:
        if looks_abbreviated(ref):
            fail(("{}: {} has no branch or tag `{}`. If it is a commit, give all 40 (or 64) characters: " +
                  "rules_tofu pins the commit, and can't look up an abbreviation without fetching the " +
                  "whole repository.").format(context, remote, ref))
        fail("{}: {} has no ref `{}`".format(context, remote, ref or "HEAD"))
    return commit

def _download_json(module_ctx, url, output, context):
    result = module_ctx.download(url = url, output = output, allow_fail = True)
    if not result.success:
        fail("Could not query the module registry for {} at {}.".format(context, url))
    return json.decode(module_ctx.read(output))

def _resolve_registry(module_ctx, parsed, version, context):
    """Where a registry module version's source is, and what pins it.

    Returns `struct(fact, location)`: the fact to record, and its location as
    `_parse_sources` reads it.
    """
    discovery = _download_json(
        module_ctx,
        "https://{}/.well-known/terraform.json".format(parsed.host),
        "registry/discovery.json",
        context,
    )
    service = modules_service_url(parsed.host, discovery)
    if service.error:
        fail(service.error)
    response = _download_json(
        module_ctx,
        registry_download_endpoint(service.url, parsed, version),
        "registry/download.json",
        context,
    )
    located = registry_location(response, context)
    if located.error:
        fail(located.error)
    location = _parse_sources(module_ctx, [located.location])[0]
    if location.error or location.kind not in ("git", "archive"):
        fail("{}: the registry gave a location rules_tofu can't fetch, `{}`: {}".format(
            context,
            located.location,
            location.error or "it is {}, not a git repository or an archive".format(
                "another registry address" if location.kind == "registry" else "a local path",
            ),
        ))
    if location.kind == "git":
        fact = {"location": located.location, "commit": _resolve_ref(module_ctx, location.remote, location.ref, context)}
    else:
        downloaded = module_ctx.download(url = location.url, output = "registry/archive")
        fact = {"location": located.location, "sha256": downloaded.sha256}
    return struct(fact = fact, location = location)

def _cached(module_ctx, key, kind):
    """The fact recorded under `key`, if it is a well-formed one of `kind`, else None."""
    fact = module_ctx.facts.get(key)
    return dict(fact) if valid_fact(fact, kind) else None

def _cached_registry(module_ctx, key):
    """A registry module's recorded fact and its location, as `_resolve_registry` returns them, or None.

    Like any other malformed fact, one whose location can't be read or whose
    pin does not match its location kind, a commit for git or a sha256 for an
    archive, is treated as a miss: the registry is queried again and the
    lockfile's record replaced (issue #96).
    """
    fact = _cached(module_ctx, key, "registry")
    if not fact:
        return None
    location = _parse_sources(module_ctx, [fact["location"]])[0]
    if location.error:
        return None
    if location.kind == "git" and "commit" in fact or location.kind == "archive" and "sha256" in fact:
        return struct(fact = fact, location = location)
    return None

def _tidy(repository_ctx, context):
    """Remove files and symlinks that Bazel must not see in the fetched tree.

    Upstream Bazel files would split the module into packages or hide
    directories via `.bazelignore`. A symlink stays only if it resolves to a
    regular file inside the repository, which Bazel reads as that file: `glob`
    skips dangling symlinks, follows directory symlinks and fails the package
    if they cycle to an ancestor, while symlinks outside the repository would
    make its contents depend on the host machine.

    If `subdir` is or lies under a directory symlink, removing that symlink
    leaves nowhere to write `//<subdir>/BUILD.bazel`, so fetching fails naming
    the symlink target.
    """
    root = repository_ctx.path(".")
    root_real = str(root.realpath)
    subdir = repository_ctx.attr.subdir
    entries = walk(root)
    for entry in entries:
        if not entry.is_dir and entry.path.basename in _BAZEL_FILES:
            repository_ctx.delete(entry.path)

    # Symlinks to directories go first: a symlink to a file through one of
    # them leads nowhere once it is gone, and goes with the rest.
    symlinks = [entry for entry in entries if entry.is_symlink and entry.path.basename not in _BAZEL_FILES]
    for entry in symlinks:
        if entry.path.exists and entry.path.is_dir:
            if subdir == entry.rel or subdir.startswith(entry.rel + "/"):
                target = str(entry.path.realpath)
                fail(("{}: `{}` is a symlink in the fetched files, to {}; rules_tofu keeps no symlinks to " +
                      "directories, so declare and call the module by the directory the link leads to").format(
                    context,
                    entry.rel,
                    target[len(root_real) + 1:] if within(target, root_real) and target != root_real else target,
                ))
            repository_ctx.delete(entry.path)
    for entry in symlinks:
        if not entry.path.exists or not within(str(entry.path.realpath), root_real):
            repository_ctx.delete(entry.path)

def _fetch(repository_ctx):
    """Fetch the module's files and tidy them.

    Returns the paths of the fetched commit's git submodules, which are not
    fetched: `modscan` fails the module if it is in one or calls into one.
    """
    attr = repository_ctx.attr
    context = "tf_modules.module `{}`".format(attr.source)
    submodules = []
    if attr.kind == "git":
        _git(repository_ctx, ["init", "-q"], context)
        fetched = repository_ctx.execute(
            ["git", "fetch", "-q", "--depth=1", attr.remote, attr.commit],
            environment = {"GIT_TERMINAL_PROMPT": "0"},
            timeout = 600,
        )
        if fetched.return_code != 0:
            # Not every server lets a commit be fetched by ID; fetch it all.
            _git(repository_ctx, ["fetch", "-q", attr.remote, "+refs/heads/*:refs/remotes/origin/*", "+refs/tags/*:refs/tags/*"], context)
        _git(repository_ctx, ["-c", "advice.detachedHead=false", "checkout", "-q", attr.commit], context)
        head = _git(repository_ctx, ["rev-parse", "HEAD"], context).strip()
        if head != attr.commit:
            fail("{}: checked out {}, expected {}".format(context, head, attr.commit))

        # A submodule is a tree entry of mode 160000, a commit of another
        # repository: `<mode> commit <id>\t<path>`.
        for line in _git(repository_ctx, ["-c", "core.quotepath=false", "ls-tree", "-r", attr.commit], context).splitlines():
            entry, _, path = line.partition("\t")
            if entry.startswith("160000 ") and path:
                submodules.append(path)
        repository_ctx.delete(".git")
    else:
        repository_ctx.download_and_extract(url = attr.url, sha256 = attr.sha256, type = attr.archive_type)
    _tidy(repository_ctx, context)
    return submodules

def _scan(repository_ctx, submodules):
    # Every declared module, this one included: it may call its own address
    # for a submodule, and another tag may declare one of its subdirectories,
    # which then provides the calls into it.
    args = [repository_ctx.path(_MODSCAN), "--self=" + repository_ctx.attr.label]
    for label, source in repository_ctx.attr.modules.items():
        args.append("--module={}={}".format(label, source))
    for path in submodules:
        args.append("--git-submodule=" + path)
    args += [repository_ctx.path("."), repository_ctx.attr.subdir]
    result = repository_ctx.execute(args)
    if result.return_code != 0:
        fail("tf_modules.module `{}`: {}".format(repository_ctx.attr.source, result.stderr.strip()))
    return json.decode(result.stdout)

def _provider_label(source, context):
    host, _, rest = source.partition("/")
    if host == "terraform.io" and rest.startswith("builtin/"):
        return None
    if host != "registry.opentofu.org":
        fail(("{} requires the provider `{}`, which rules_tofu can't vendor: it only vendors providers " +
              "from registry.opentofu.org").format(context, source))
    return "@{}//{}".format(_PROVIDERS_HUB, rest)

def _tf_module_repo_impl(repository_ctx):
    attr = repository_ctx.attr
    context = "tf_modules.module `{}`".format(attr.source)
    scanned = _scan(repository_ctx, _fetch(repository_ctx))

    if scanned["undeclared"]:
        fail("{} calls modules that MODULE.bazel doesn't declare. Add:\n{}".format(
            context,
            "\n".join(["    {}  # called at {}".format(c["tag"], c["where"]) for c in scanned["undeclared"]]),
        ))

    providers = [p for p in [_provider_label(p, context) for p in scanned["providers"]] if p]
    package = attr.subdir
    data = ['glob(["**"], exclude = {})'.format(repr(_EXCLUDED))]
    if package:
        # The module's library lives in its own directory, which is what calls
        # reach, but local paths may lead out of it into the rest of the files.
        data.append(repr(["//:" + _FILES_TARGET]))
        repository_ctx.file(
            "BUILD.bazel",
            content = """\
filegroup(
    name = {name},
    srcs = glob(["**"], exclude = {excluded}),
    visibility = ["//:__subpackages__"],
)
""".format(name = repr(_FILES_TARGET), excluded = repr(_EXCLUDED)),
            executable = False,
        )

    # Only the hub may see the library: BUILD files name it as
    # `@tf_modules//<path>`.
    repository_ctx.file(
        (package + "/" if package else "") + "BUILD.bazel",
        content = """\
load("@rules_tofu//tf:defs.bzl", "tf_library")

tf_library(
    name = {name},
    data = {data},
    fmt_test = False,
    module_source = {source},
    module_version = {version},
    providers = {providers},
    visibility = ["@{hub}//:__subpackages__"],
    deps = {deps},
)
""".format(
            name = repr(_MODULE_TARGET),
            data = " + ".join(data),
            source = repr(attr.source),
            version = repr(attr.version),
            providers = repr(providers),
            deps = repr(scanned["deps"]),
            hub = _HUB,
        ),
        executable = False,
    )

    # The files came from a git commit or an archive checked against `sha256`,
    # and modscan is an input Bazel records, so the attributes determine the
    # contents.
    return reproducible(repository_ctx)

_tf_module_repo = repository_rule(
    implementation = _tf_module_repo_impl,
    attrs = {
        "source": attr.string(mandatory = True),
        "version": attr.string(),
        "label": attr.string(mandatory = True),
        "kind": attr.string(mandatory = True, values = ["git", "archive"]),
        "remote": attr.string(),
        "commit": attr.string(),
        "url": attr.string(),
        "sha256": attr.string(),
        "archive_type": attr.string(),
        # The module's directory in the fetched files.
        "subdir": attr.string(),
        # Every declared module: label -> source, for matching the calls this
        # module makes.
        "modules": attr.string_dict(),
    },
)

def _tf_modules_hub_impl(repository_ctx):
    repository_ctx.file("BUILD.bazel", "", executable = False)
    for path, target in repository_ctx.attr.modules.items():
        repository_ctx.file(
            path + "/BUILD.bazel",
            content = """\
alias(
    name = "{name}",
    actual = "{target}",
    visibility = ["//visibility:public"],
)
""".format(name = path.rpartition("/")[2], target = target),
            executable = False,
        )

_tf_modules_hub = repository_rule(
    implementation = _tf_modules_hub_impl,
    attrs = {
        # `<path>` -> the module's library.
        "modules": attr.string_dict(),
    },
)

_module_tag = tag_class(
    doc = "Declare an external OpenTofu module from a registry, git repository or archive " +
          "for offline use as a `tf_library` under `@tf_modules`.",
    attrs = {
        "source": attr.string(
            mandatory = True,
            doc = "HCL module `source` address. Registry sources take " +
                  "`[<host>/]<namespace>/<name>/<system>` without `//<subdir>`. Git sources " +
                  "take `git::<url>`, `git://...`, `github.com/<owner>/<repo>` or " +
                  "`git@<host>:<path>`, with optional `//<subdir>` and `?ref=` or `?depth=`. " +
                  "Archive sources take `http://`, `https://` or `file://` URLs ending in " +
                  "`.zip`, `.tar.gz`, `.tgz`, `.tar.bz2`, `.tar.tbz2`, `.tar.xz` or `.txz`, " +
                  "or carrying `?archive=<type>`, with optional `//<subdir>`. S3 via `s3::`, " +
                  "GCS via `gcs::`, Bitbucket via `bitbucket.org/`, Mercurial via `hg::` and " +
                  "`X-Terraform-Get` redirects are not supported.",
        ),
        "version": attr.string(
            doc = "Exact module version as `X.Y.Z`, e.g. `\"5.8.1\"`; constraints and " +
                  "prereleases such as `\"1.2.0-rc1\"` are rejected. Required for registry " +
                  "modules and disallowed for git or archive sources.",
        ),
        "name": attr.string(
            doc = "Module label under `@tf_modules//<name>`, using only ASCII letters, digits, " +
                  "`_`, `-` and `.`. Required for git and archive sources. Disallowed for " +
                  "registry modules, which use `@tf_modules//<namespace>/<name>/<system>` in " +
                  "lower case, prefixed with `<host>/` for hosts other than " +
                  "`registry.opentofu.org`, e.g. `@tf_modules//example.com/acme/vpc/aws`.",
        ),
        "commit": attr.string(
            doc = "Full 40- or 64-character lowercase hex commit SHA to fetch for a git source; " +
                  "abbreviated commit hashes are rejected in both `commit` and `?ref=`. When " +
                  "unset, the commit named by `?ref=`, or `HEAD` when omitted, is resolved on " +
                  "first fetch and recorded in `MODULE.bazel.lock`. Disallowed for non-git " +
                  "sources.",
        ),
        "sha256": attr.string(
            doc = "SHA256 digest of the archive. Required for archive sources and disallowed " +
                  "otherwise.",
        ),
    },
)

def _check_tag(tag, parsed, context):
    """Return an error if `tag`'s attributes don't suit its kind of source, else None."""
    if parsed.kind == "local":
        return "{}: a local path is not a module to fetch; use a tf_library for it".format(context)
    if parsed.kind == "registry" and parsed.subdir:
        return ("{}: declare the module itself, without `//<subdir>`; calls reach its " +
                "submodules with `//<subdir>` anyway").format(context)
    allowed = {
        "registry": ["version"],
        "git": ["name", "commit"],
        "archive": ["name", "sha256"],
    }[parsed.kind]
    for key in ["version", "name", "commit", "sha256"]:
        if getattr(tag, key) and key not in allowed:
            return "{}: `{}` doesn't apply to a {} source".format(context, key, parsed.kind)
    if parsed.kind == "registry":
        if not tag.version:
            return "{}: a registry module needs an exact `version`".format(context)
        error = check_version(tag.version)
        if error:
            return "{}: `version` {}".format(context, error)
        return None
    error = check_name(tag.name)
    if error:
        return "{}: `name` {}".format(context, error)
    if parsed.kind == "archive" and not tag.sha256:
        return "{}: an archive needs its `sha256`".format(context)
    if tag.commit and not is_commit(tag.commit):
        return "{}: `commit` must be a full commit ID, got `{}`".format(context, tag.commit)
    if tag.commit and is_commit(parsed.ref) and parsed.ref != tag.commit:
        return "{}: `commit` {} differs from the source's `?ref=`".format(context, tag.commit)
    return None

def _resolve(tags, path):
    """Pick the one tag that declares the module at `path`, from every module's."""
    kind = tags[0].parsed.kind
    if kind == "registry":
        root_versions = sorted({t.tag.version: True for t in tags if t.is_root}.keys())
        if len(root_versions) > 1:
            fail("The root module declares `@tf_modules//{}` at more than one version: {}".format(path, ", ".join(root_versions)))
        chosen = None
        for t in tags:
            if root_versions and t.tag.version != root_versions[0]:
                continue
            if chosen == None or version_gt(t.tag.version, chosen.tag.version):
                chosen = t
        return chosen

    for t in tags:
        if t.tag.source != tags[0].tag.source:
            fail("`@tf_modules//{}` is declared with different sources: `{}` (module `{}`) and `{}` (module `{}`)".format(
                path,
                tags[0].tag.source,
                tags[0].mod_name,
                t.tag.source,
                t.mod_name,
            ))
    for key in ["commit", "sha256"]:
        values = {getattr(t.tag, key): t.mod_name for t in tags if getattr(t.tag, key)}
        if len(values) > 1:
            fail("`@tf_modules//{}`: modules disagree on `{}` ({})".format(
                path,
                key,
                ", ".join(["`{}` from `{}`".format(v, m) for v, m in values.items()]),
            ))
    pinned = [t for t in tags if t.tag.commit or t.tag.sha256]
    return pinned[0] if pinned else tags[0]

def _tf_modules_extension_impl(module_ctx):
    root_repos = []
    tagged = []
    for mod in module_ctx.modules:
        if mod.is_root:
            root_repos.append(_HUB)
        for tag in mod.tags.module:
            tagged.append(struct(tag = tag, mod = mod))

    by_path = {}
    parsed_sources = _parse_sources(module_ctx, [t.tag.source for t in tagged]) if tagged else []
    for t, parsed in zip(tagged, parsed_sources):
        tag, mod = t.tag, t.mod
        context = "tf_modules.module `{}` in module `{}`".format(tag.source, mod.name)
        if parsed.error:
            fail("{}: {}".format(context, parsed.error))
        error = _check_tag(tag, parsed, context)
        if error:
            fail(error)
        path = hub_path(parsed) if parsed.kind == "registry" else tag.name
        if path not in by_path:
            by_path[path] = []
        elif by_path[path][0].parsed.kind != parsed.kind:
            fail("`@tf_modules//{}` is declared both as a {} module and as a {} module".format(path, by_path[path][0].parsed.kind, parsed.kind))
        by_path[path].append(struct(tag = tag, parsed = parsed, is_root = mod.is_root, mod_name = mod.name))

    repos = {}
    for path in by_path:
        name = repo_name(path)
        if name in repos:
            fail("`@tf_modules//{}` and `@tf_modules//{}` would share the repository name `{}`".format(repos[name], path, name))
        repos[name] = path

    facts = {}
    resolved = {}
    for path, tags in by_path.items():
        chosen = _resolve(tags, path)
        tag, parsed = chosen.tag, chosen.parsed
        context = "tf_modules.module `{}`".format(tag.source)
        if parsed.kind == "registry":
            key = "{}@{}".format(path, tag.version)
            pinned = _cached_registry(module_ctx, key) or _resolve_registry(module_ctx, parsed, tag.version, context)
            fact, location = pinned.fact, pinned.location
            facts[key] = fact
            resolved[path] = struct(
                source = tag.source,
                version = tag.version,
                kind = location.kind,
                remote = location.remote,
                commit = fact.get("commit", ""),
                url = location.url,
                sha256 = fact.get("sha256", ""),
                archive_type = location.type,
                subdir = location.subdir,
            )
        elif parsed.kind == "git":
            commit = tag.commit
            if not commit:
                fact = _cached(module_ctx, tag.source, "git") or {"commit": _resolve_ref(module_ctx, parsed.remote, parsed.ref, context)}
                facts[tag.source] = fact
                commit = fact["commit"]
            resolved[path] = struct(
                source = tag.source,
                version = "",
                kind = "git",
                remote = parsed.remote,
                commit = commit,
                url = "",
                sha256 = "",
                archive_type = "",
                subdir = parsed.subdir,
            )
        else:
            resolved[path] = struct(
                source = tag.source,
                version = "",
                kind = "archive",
                remote = "",
                commit = "",
                url = parsed.url,
                sha256 = tag.sha256,
                archive_type = parsed.type,
                subdir = parsed.subdir,
            )

    labels = {path: "@{}//{}".format(_HUB, path) for path in resolved}
    declared = {labels[path]: r.source for path, r in resolved.items()}
    for path, r in resolved.items():
        _tf_module_repo(
            name = repo_name(path),
            label = labels[path],
            source = r.source,
            version = r.version,
            kind = r.kind,
            remote = r.remote,
            commit = r.commit,
            url = r.url,
            sha256 = r.sha256,
            archive_type = r.archive_type,
            subdir = r.subdir,
            modules = declared,
        )

    # Always created, so that BUILD files in any module can name modules as
    # `@tf_modules//<path>` however the root declares them.
    _tf_modules_hub(
        name = _HUB,
        modules = {
            path: "@{}//{}:{}".format(repo_name(path), r.subdir, _MODULE_TARGET)
            for path, r in resolved.items()
        },
    )

    # `reproducible = True`: every fetch is pinned by a commit or sha256, on a
    # tag or in `facts`, so the repos are a pure function of the module graph
    # and the facts. Returning only this evaluation's facts drops stale ones.
    non_dev = module_ctx.root_module_has_non_dev_dependency
    return module_ctx.extension_metadata(
        root_module_direct_deps = root_repos if non_dev else [],
        root_module_direct_dev_deps = [] if non_dev else root_repos,
        reproducible = True,
        facts = facts,
    )

tf_modules = module_extension(
    implementation = _tf_modules_extension_impl,
    tag_classes = {"module": _module_tag},
    doc = """Fetches external OpenTofu modules and exposes each as a `tf_library` under `@tf_modules`.

Registry modules are labelled `@tf_modules//<namespace>/<name>/<system>` in
lower case, or `@tf_modules//<host>/<namespace>/<name>/<system>` for registries
other than `registry.opentofu.org`. Custom hosts must advertise `modules.v1` at
`/.well-known/terraform.json` and return a git or archive download location.
Git and archive modules are labelled `@tf_modules//<name>`. Submodules are
called in HCL with `//<subdir>` while depending on the parent module's label.

Every fetch is pinned:
  - Registry modules record their download location and git commit or archive
    `sha256` in `MODULE.bazel.lock`.
  - Git modules fetch `commit` when set, or resolve `?ref=`, defaulting to
    `HEAD`, on first fetch and record the commit in `MODULE.bazel.lock`.
    Fetching git modules requires `git` on `PATH`.
  - Archive modules verify `sha256`.

Unpacking deletes upstream Bazel files: `.bazelignore`, `BUILD`, `BUILD.bazel`,
`MODULE.bazel`, `REPO.bazel`, `WORKSPACE` and `WORKSPACE.bazel`. It also
excludes `.git/`, `.terraform/`, `.terraform.lock.hcl`, `terraform.tfvars`,
`terraform.tfvars.json`, `*.auto.tfvars` and `*.auto.tfvars.json`, and removes
directory, dangling or external symlinks; `//<subdir>` cannot be a symlink. Git
submodules are not fetched.

Each generated `tf_library` propagates providers from the module's root, its
`modules/` subdirectories, and directories it calls itself. If only a caller
reaches a `//<subdir>` outside `modules/`, declare that submodule's providers on
the caller. External modules called transitively must also be declared with
`tf_modules.module`.

Across the module graph, each `@tf_modules` path resolves once. For registry
modules, the root module's tag wins, else the highest version. Git and archive
declarations sharing a `name` must agree on `source` and must not disagree on
`commit` or `sha256`; an explicit `commit` wins over an unset one.

`tf_modules` scans sources and fetched modules with a Go helper compiled by
`rules_go`'s host Go SDK, honouring `GOPROXY`, `GONOSUMDB`, `GOPRIVATE`,
`GONOPROXY` and `GOSUMDB`; workspaces that register a Go SDK must select a
version at least as new as the `go` directive in `rules_tofu`'s `go.mod`.

Import only `tf_modules` with `use_repo`. See
[From a registry, git or an archive](https://github.com/benbirt/rules_tofu#from-a-registry-git-or-an-archive).
""",
)
