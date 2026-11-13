"""Bzlmod module extension that fetches OpenTofu modules and exposes each as a
`tf_library`, to call from HCL by its `source` without `tofu init` fetching
anything.

Downstream usage in MODULE.bazel:

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

and in a BUILD file:

    tf_library(
        name = "network",
        srcs = ["main.tf"],
        deps = ["@tf_modules//terraform-aws-modules/vpc/aws"],
    )

where main.tf calls the module by the same `source` (with any `version`
constraint, which must accept the pinned version), or one of its submodules
with `//<subdir>`.

Each module is labelled through the `@tf_modules` hub: a registry module as
`@tf_modules//<namespace>/<name>/<system>` (lower-cased, and prefixed with the
host unless it is registry.opentofu.org), any other as
`@tf_modules//<name>`, from the tag's `name`. Behind each alias is a private
repository holding the module's files and a `tf_library` whose
`module_source` is the tag's `source`.

Where the files come from, and what pins them:

- A **registry module** needs an exact `version`. The registry says where its
  source is, which for registry.opentofu.org is a git commit. The location
  and commit are recorded as facts in MODULE.bazel.lock, so the registry is
  asked once per lockfile.
- A **git source** is fetched with `git` at a commit: `commit` if the tag
  sets it, else the one `?ref=` names now, which is recorded as a fact
  (trust on first use). Either way a tag that moves later changes nothing.
- An **archive** needs its `sha256`.

The generated library carries the module's providers (its required_providers
as `@tf_providers//<namespace>/<name>`), which the root module pins as usual,
and depends on the modules it calls, which must be declared too: a module
that calls an undeclared one fails when fetched, naming the tag to add (its
calls to its own address, for a submodule, it provides itself). Each
module has one version per build. For a registry module the root's tag wins,
else the highest version requested; git and archive modules must be declared
identically wherever their `name` appears.
"""

load("//tf/private:fetched_tree.bzl", "walk", "within")
load("//tf/private:repo_metadata.bzl", "reproducible")
load(
    ":source.bzl",
    "check_name",
    "check_version",
    "hub_path",
    "is_commit",
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
# own, so they're removed before the module's BUILD file is written.
_BAZEL_FILES = ["BUILD", "BUILD.bazel", "MODULE.bazel", "REPO.bazel", "WORKSPACE", "WORKSPACE.bazel"]

# The generated targets: the module's library, in its directory, and, when
# that is a subdirectory, the rest of the fetched files, which local paths
# may lead out into. Named so that no file of the module can share the name:
# a file `files` beside a `filegroup(name = "files")` fails the package.
_MODULE_TARGET = "_rules_tofu_module"
_FILES_TARGET = "_rules_tofu_files"

# Files that never belong in a work tree: VCS metadata; tofu's own state and
# lock, which a module repository may have committed; and the variables files
# tofu loads by itself, which a module's root and its examples often carry
# and no rule accepts as data (`check_no_auto_tfvars`). tofu reads none of
# them from a module that is called.
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
        fail("{}: {} has no ref `{}`".format(context, remote, ref or "HEAD"))
    return commit

def _download_json(module_ctx, url, output, context):
    result = module_ctx.download(url = url, output = output, allow_fail = True)
    if not result.success:
        fail("Could not query the module registry for {} at {}.".format(context, url))
    return json.decode(module_ctx.read(output))

def _resolve_registry(module_ctx, parsed, version, context):
    """Where a registry module version's source is, and what pins it: a fact."""
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
    if location.error or location.kind == "registry":
        fail("{}: the registry gave a location rules_tofu can't fetch, `{}`: {}".format(context, located.location, location.error or "it is another registry address"))
    if location.kind == "git":
        return {"location": located.location, "commit": _resolve_ref(module_ctx, location.remote, location.ref, context)}
    downloaded = module_ctx.download(url = location.url, output = "registry/archive")
    return {"location": located.location, "sha256": downloaded.sha256}

def _cached(module_ctx, key):
    fact = module_ctx.facts.get(key)
    return dict(fact) if valid_fact(fact) else None

def _tidy(repository_ctx, context):
    """Remove what the fetched files hold that Bazel must not see.

    Bazel's own files would split the module into packages of its own. A
    symlink stays only if it leads to a file of the module, which Bazel
    reads as that file. Its `glob` skips a symlink to nothing, but follows
    one to a directory and fails the package when the links lead back up to
    an ancestor; and through a symlink out of the module, the module's files
    would depend on the machine's.

    The module's own directory can't be a symlink then: `//<subdir>` is
    where its library is written, so that fails, naming where the link leads.
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
    attr = repository_ctx.attr
    context = "tf_modules.module `{}`".format(attr.source)
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
        repository_ctx.delete(".git")
    else:
        repository_ctx.download_and_extract(url = attr.url, sha256 = attr.sha256, type = attr.archive_type)
    _tidy(repository_ctx, context)

def _scan(repository_ctx):
    # Every declared module, this one included: it may call its own address
    # for a submodule, and another tag may declare one of its subdirectories,
    # which then provides the calls into it.
    args = [repository_ctx.path(_MODSCAN), "--self=" + repository_ctx.attr.label]
    for label, source in repository_ctx.attr.modules.items():
        args.append("--module={}={}".format(label, source))
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
    _fetch(repository_ctx)
    scanned = _scan(repository_ctx)

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
    attrs = {
        "source": attr.string(
            mandatory = True,
            doc = "The module's address, as HCL's `source` gives it: a registry address " +
                  "(`[<host>/]<namespace>/<name>/<system>`), `git::<url>[//<subdir>][?ref=<ref>]`, " +
                  "`github.com/<owner>/<repo>[//<subdir>][?ref=<ref>]`, or the URL of an archive.",
        ),
        "version": attr.string(
            doc = "For a registry module, the exact version to pin, as `X.Y.Z`, e.g. \"5.8.1\" " +
                  "(no constraint and no prerelease suffix). Required for registry modules and " +
                  "not allowed for other sources.",
        ),
        "name": attr.string(
            doc = "For a git or archive source, the module's label: `@tf_modules//<name>`. " +
                  "Required for those and not allowed for registry modules, whose label is " +
                  "their address in lower case, `@tf_modules//<namespace>/<name>/<system>`, " +
                  "with the host in front unless it is registry.opentofu.org.",
        ),
        "commit": attr.string(
            doc = "For a git source, the commit to fetch. If unset, the commit that `?ref=` " +
                  "names (the default branch without one) is recorded in MODULE.bazel.lock " +
                  "the first time, and used from then on.",
        ),
        "sha256": attr.string(
            doc = "For an archive, its SHA256. Required for archives and not allowed otherwise.",
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
            fact = _cached(module_ctx, key) or _resolve_registry(module_ctx, parsed, tag.version, context)
            facts[key] = fact
            location = _parse_sources(module_ctx, [fact["location"]])[0]
            if location.error or location.kind == "registry":
                fail("{}: MODULE.bazel.lock records a location rules_tofu can't fetch, `{}`".format(context, fact["location"]))
            if location.kind == "git" and "commit" not in fact or location.kind == "archive" and "sha256" not in fact:
                fail("{}: MODULE.bazel.lock's record of it is incomplete; delete it from the file".format(context))
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
                fact = _cached(module_ctx, tag.source) or {"commit": _resolve_ref(module_ctx, parsed.remote, parsed.ref, context)}
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
    doc = "Fetches OpenTofu modules as `tf_library` targets, `@tf_modules//<path>`. See the module docstring.",
)
