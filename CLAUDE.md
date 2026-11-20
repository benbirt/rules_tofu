# Notes for Claude Code

Public docs are in `README.md`. This file is the agent-specific overlay:
sandbox bootstrap, hard invariants, repo conventions. Don't add general
documentation here.

Respond in British English.

## Build / test in the Anthropic sandbox

`bcr.bazel.build` is not in the proxy allowlist. Before the first build,
write `.bazelrc.local` (gitignored; the committed `.bazelrc` already
`try-import`s it) using the llmbox bootstrap script. Run once per session:

```python
import os

proxy = os.environ.get("GLOBAL_AGENT_HTTP_PROXY", "")

lines = [
    "# Auto-generated — do not commit (gitignored)",
    "",
    "common --registry=https://raw.githubusercontent.com/bazelbuild/bazel-central-registry/main",
    "",
    "startup --host_jvm_args=-Djavax.net.ssl.trustStore=/etc/ssl/certs/java/cacerts",
    "startup --host_jvm_args=-Djavax.net.ssl.trustStorePassword=changeit",
]

if proxy:
    no_scheme = proxy[len("http://"):]
    userinfo, hostport = no_scheme.rsplit("@", 1)
    host, port = hostport.rsplit(":", 1)
    user, password = userinfo.split(":", 1)
    lines += [
        "",
        f"startup --host_jvm_args=-Dhttps.proxyHost={host}",
        f"startup --host_jvm_args=-Dhttps.proxyPort={port}",
        f"startup --host_jvm_args=-Dhttps.proxyUser={user}",
        f"startup --host_jvm_args=-Dhttps.proxyPassword={password}",
        f"startup --host_jvm_args=-Dhttp.proxyHost={host}",
        f"startup --host_jvm_args=-Dhttp.proxyPort={port}",
        f"startup --host_jvm_args=-Dhttp.proxyUser={user}",
        f"startup --host_jvm_args=-Dhttp.proxyPassword={password}",
        "startup --host_jvm_args=-Dhttp.nonProxyHosts=localhost|127.*|[::1]",
        "",
        "startup --host_jvm_args=-Djdk.http.auth.tunneling.disabledSchemes=",
        "startup --host_jvm_args=-Djdk.http.auth.proxying.disabledSchemes=",
    ]

with open(".bazelrc.local", "w") as f:
    f.write("\n".join(lines) + "\n")
```

Then `bazel shutdown` so the new startup flags take effect.

- Starlark unit tests (`//toolchain/tests/...`, `//tf/providers/tests/...`,
  `//tf/modules/tests/...`) and `//tofu` need no Go toolchain.
- Everything else (`bazel build //...`, `tf_library`, `tf_deploy`, `tf_test`,
  Bazel Go tests, and workspaces using `from_lock` or `tf_modules`) needs
  `rules_go` to download the Go SDK for `//tf/private/cmd/modules`, `dupcheck`,
  `runner` and `@rules_tofu_host_tools`. That failed in the sandbox when
  `rules_go` fetched `https://go.dev/dl/?mode=json` (proxy 403); CI has open
  network and is unaffected.
- Outside Bazel, run Go unit tests with `go test ./tf/private/cmd/...` (in
  Bazel: `bazel test //tf/private/cmd/...`). Plain `go test` skips the `runner`,
  `dupcheck`, `modules` and `tofushow` tests and still reports `ok` unless
  `-runner=<path>`, `-dupcheck=<path>` and `-tofu=<absolute path>`
  (`tofutest.Reader`) are passed:
  - `go build -o /tmp/runner ./tf/private/cmd/runner && go test ./tf/private/cmd/runner -runner=/tmp/runner`
  - `go build -o /tmp/dupcheck ./tf/private/cmd/dupcheck && go test ./tf/private/cmd/dupcheck -dupcheck=/tmp/dupcheck`
  - `go test ./tf/private/cmd/internal/tofushow ./tf/private/cmd/modules -tofu=/abs/path/to/tofu`

## Where things are documented

- `README.md` is the user documentation: rules, providers ("Providers"),
  modules ("Modules"), the toolchain ("The `tofu` binary"), work trees and the
  runner ("How it works"), "Restrictions", "Non-goals" and "Known gaps".
- `CONTRIBUTING.md` is the maintainer documentation: repository layout, short
  hard invariants and public `.bzl` entry points, running tests, formatting
  and lint checks, pinning an OpenTofu release, the Go version floor, and
  releasing to the BCR.
- This file holds only what belongs in neither: the sandbox bootstrap above,
  the reasons behind the invariants, and traps earlier changes hit. Move
  descriptions of how the code works to `README.md` or `CONTRIBUTING.md` and
  leave a pointer. Names in backticks are for `grep`.

## Invariants — don't regress these

### Providers and the lock

- Read only the root module's `from_lock` `.terraform.lock.hcl` (`README.md`,
  "Pinning with a lock file"; `CONTRIBUTING.md`, invariant 1):
  - Reject `.terraform.lock.hcl` in every directory of a work tree at analysis
    in `materialize` (`tf/private/work_tree.bzl`; `lock_file_smuggled`).
    `ALLOWED_SRC_EXTS` admits `.hcl` and `data` allows all files, so
    `materialize` is the check across `srcs`, `data` and `deps`.
  - Pass the managed lock via `TfProviderInfo.lock` (`--lock-file` on the
    runner). `prepare` copies it into the run directory (`installLockFile`) and
    never into `<name>.work`.
- Don't pass `-lockfile=readonly` to `tofu init`: it fails when the lock has
  entries a work tree doesn't use.
- Pin `registry.opentofu.org` providers with the root module's lock only, and
  reserve `tf_providers.provider` for off-registry providers (issue #147;
  `README.md`, "Providers"). Root tags require `source`, `version`,
  `url_template` and `sha256` (`check_tag`). Don't reintroduce tags for
  registry providers, cross-module provider version resolution, provider facts
  in `MODULE.bazel.lock`, or registry requests in the extension:
  `.terraform.lock.hcl` is OpenTofu's own lock mechanism and `pin` writes it.
- Verify every provider download against a per-platform digest: the tag's
  `sha256` or a registry digest vouched for by the lock's `zh:` hashes
  (`lock_vouches`).
- Resolve a locked provider's digests in its repository rule
  (`_locked_download`), never in the extension and never as facts. The lock
  alone pins it, so a bot updating `versions.tf` and `.terraform.lock.hcl`
  leaves `MODULE.bazel.lock` valid (`//tests/integration:lock_update_test`).
- Set `reproducible = True` on all three extensions (`tofu`, `tf_providers`,
  `tf_modules`). Every repository they declare is determined by the module
  graph, watched lock file, `versions.bzl` table and recorded facts, so copying
  repo specs into `MODULE.bazel.lock` pins nothing extra, and returning only
  active facts lets stale entries drop out.
- Validate extension facts on read (`valid_fact`, `_cached` and
  `_cached_registry` in `tf_modules`), treating any bad fact as a cache miss to
  re-resolve and never calling `fail()` (issue #96). `MODULE.bazel.lock` is a
  checked-in file that can outlive the code that wrote it or be mis-merged.
  `valid_fact` rejects a well-formed fact stored under a key of the other kind.
  `_cached_registry` also rejects a registry fact whose `location` fails to
  parse or doesn't match its `commit` or `sha256` pin.
- End download repository rules (`_tofu_download`, `_tf_provider_download`,
  `_tf_module_files`) with `return reproducible(repository_ctx)`
  (`tf/private/repo_metadata.bzl`). Bazel 9's repository contents cache then
  shares fetches across output bases, so the nested integration tests fetch
  each provider, `tofu` binary and module once per machine. Declaring it is
  sound only while every download those rules make is checked against a digest
  carried by their attributes or the lock: the registry supplies URLs, never
  contents. Any rule that fetches anything unpinned must stop declaring it. Omit
  it on the cheap hub repositories and on `_tf_module_repo`, whose symlinks are
  absolute paths into one output base.
- Silently ignore `from_lock` and `provider` tags in dependencies (`README.md`,
  "Providers"). A dependency carries them from its own development as a root
  module; in a consumer workspace the root owns provider pins and libraries
  only constrain versions. A provider tagged only by dependencies gets a
  `@tf_providers//<ns>/<name>` package that fails to load (`ignored_tag_error`,
  `dependency_tag_ignored`).
- Don't set `mandatory = True` on `url_template` or `sha256` on `_provider_tag`.
  Bazel enforces `mandatory` on every module's tags, so a dependency's 0.5 tag
  without them would fail to load before the root module could ignore it.
  Validate root tags in `check_tag` instead; a root tag without `url_template`
  fails with instructions to move the provider to the lock (`lock_update_test`
  step 6).
- Keep `lock_file` on `tf_providers.from_lock` named `lock_file` and
  `mandatory = True` with no default, and tell users in docs and `tf_pin` to
  write it as a string literal (issue #131; `lock_update_test` step 5).
  Dependabot's `bazel` updater does not clone the repository, fetching only
  files that `MODULE.bazel` names as string literals in attributes it
  recognises alongside their `BUILD` files. It then runs `bazel mod tidy` to
  regenerate `MODULE.bazel.lock`, which evaluates `from_lock`.
- Always create the `@tf_providers` hub so dependencies' `use_repo` calls
  resolve even when the root declares no providers.
- Treat a missing lock file as empty so `pin` can write the first one.
- Record an unusable lock entry or a lock/tag conflict (`_lock_provider_repos`,
  issue #85) in the hub's `errors` so `@tf_providers//<ns>/<name>` fails to
  load when used; never call `fail()` in the extension. Failing
  the extension would also break `@tf_providers//:pin`. When reading an
  existing lock file, only a malformed file fails `_read_lock`: bad HCL, an
  invalid provider address, a duplicate provider block, or an entry missing
  `version`.
- Parse lock files with `lockjson` (`tf/private/cmd/lockjson`, `gohcl`) and
  OpenTofu's registry-address library, and validate entry fields in
  `lock_entries` (`tf/providers/lock.bzl`). Don't write Starlark HCL or
  address parsers.
- Compile `lockjson` and `modscan` with the host Go SDK in
  `tf/private/host_tools.bzl` (`@rules_tofu_host_tools`), a non-dev
  `use_repo_rule`, because extensions cannot run Bazel-built targets. The rule
  watches `_SOURCES` and builds under `-mod=readonly` via `GOPROXY`, verifying
  Go modules against `rules_tofu`'s `go.sum`, so keep `_SOURCES`, `go.mod` and
  `go.sum` tidy.
- `@tf_providers//:pin` (`tf/private/cmd/pin`; `README.md`, "Providers"):
  - Discover `required_providers` with one `bazel query`, never a filesystem
    walk, which would miss `.bazelignore`, deleted packages and nested
    workspaces. `$BAZEL` overrides the `bazel` binary.
  - Pass `--output=streamed_jsonproto` and `--keep_going`, and accept exit 3
    because `deps()` traverses `providers` into `@tf_providers` packages that
    don't exist yet.
  - Count every configuration file the query returns, in any repository
    (issues #60, #123 and #140). In an external repository they are all some
    library's `srcs`, whose providers that library lists, and Bazel loads each of those labels whether
    or not anything calls the directory. `tf_modules` keeps a fetched module's
    examples out of every target, and so out of the lock (`tf_modules_test`).
    Don't bring back a filter on the directories a library serves: it left a
    declared submodule that nothing called unlocked, which then failed to
    load.
  - Combine constraints by provider source (`combine`), both within a directory
    in `requirements` and across directories, never by local name. Merging by
    local name assumes a single module: `bazel query` returns files rather than
    targets, and two targets in one package may share a local name (issue #99).
  - Write a fresh scratch `main.tf.json` (`writeScratch`) containing one root
    `required_providers` block for all non-excluded providers
    (`--exclude-source`), and run `tofu providers lock` there. Extracting only
    `required_providers` avoids loading resources or module calls out of
    context. Don't bring back a child module per directory or `tofu get` (issue
    #151): since issue #99 `pin` combines by source under synthetic local names
    (`localNames`), so child-module nesting carries no information. Running in
    a fresh directory works around `tofu providers lock` having no `-upgrade`
    flag and otherwise keeping existing versions. When `--exclude-source`
    leaves no providers, write an empty header-only lock so moving a provider
    to a root tag clears the conflict.
  - Don't verify the lock's `constraints` attribute: Renovate and Dependabot
    rewrite it in their own form.
- Vendor the full unpacked provider zip as `TfProviderInfo.files` and symlink
  every file in `materialize_plugin_tree`. Lock `h1:` hashes cover the whole
  package directory, so a binary-only plugin tree fails `tofu init`. Don't
  single out the provider binary (issue #145): the rules never execute it
  directly, and `tofu init -plugin-dir` locates it in the directory. An archive
  that unpacks to no files fails its fetch; one with zero or multiple binaries
  fails at `tofu init`.
- Resolve provider zip URLs through the pure functions in
  `tf/providers/source.bzl` (`//tf/providers/tests`): `url_template` for tagged
  providers, or the OpenTofu registry for locked providers across every
  namespace including `hashicorp/`. OpenTofu canonicalises bare `<ns>/<name>`
  addresses to `registry.opentofu.org` and looks for plugins under that host,
  and `releases.hashicorp.com` builds have different checksums, so don't
  special-case it.
- Lower-case all provider sources to match OpenTofu's canonical addresses.
- Declare one `tf_providers_<ns>_<name>_<os>_<arch>` repository per provider
  and platform (issue #146). `@tf_providers//<ns>/<name>` aliases them via a
  `select()` over `config_setting`s in the hub so a build fetches only its
  execution platform's repository. `PLATFORMS` in `toolchain/versions.bzl` is
  the single platform list for `tofu` and providers. A locked provider declares
  every platform in `PLATFORMS` because lock hashes carry no platform names; an
  unpublished platform fails when fetched. A tagged provider declares the keys
  of `sha256`; other platforms fail analysis via `no_match_error`
  (`missing_platform_error`, `missing_platform`).
- Keep those per-platform repositories private: omit them
  from direct deps, keep their files private, and expose `:provider` only to
  the hub. Only `@tf_providers//<ns>/<name>` is public; don't reintroduce
  per-provider labels, so downstream `BUILD` files stay independent of how the
  root pins providers and `bazel mod tidy` ignores the private repos.

### Modules

- Never fetch modules at build or run time (`README.md`, "How it works"). Every
  rule writes `.terraform/modules/modules.json` via `//tf/private/cmd/modules`
  and runs `tofu init -get=false`.
- Don't drop `-get=false`: it stops `tofu` silently re-downloading on a
  `version` mismatch and makes local calls use the manifest. Because the
  manifest format is internal to `tofu`, `module_dependency_<version>_test`
  runs for every entry in `KNOWN_VERSIONS`.
- Read configuration modules in `//tf/private/cmd/modules` (`TofuModules`) only
  through `tofushow.Reader.ReadFiles` (`tf/private/cmd/internal/tofushow`;
  issue #153):
  - Never use `tfconfig.ReadCalls` or `tfconfig.ReadRequirements` in the
    manifest tool; those stay in `tfconfig` only for `modscan` and `pin` until
    stages 2 and 3 of issue #153. Test files' `run` blocks still use
    `tfconfig.TestFiles` and `ReadTestCalls` because OpenTofu has no command
    that reports them.
  - Always stage into a fresh scratch directory under `<scratch>/<dir>` and run
    `tofu show -json -no-color -module=<dir>` with `<scratch>` as the working
    directory, stripping every `TF_*` variable, `HOME` and `XDG_CONFIG_HOME`
    and setting `TF_IN_AUTOMATION=1` and `HOME=<scratch>` (`environ`;
    `TestReadFiles_IgnoresEnvironment`). Never run `tofu show -module` in the
    work tree: `tofu show` loads `terraform.tfvars[.json]` and
    `*.auto.tfvars[.json]` from its working directory (not the `-module`
    directory) and applies `TF_VAR_*` to the shown module, which would alter an
    evaluated `source`, while `TF_LOG*` and CLI config in `HOME` or
    `TF_CLI_CONFIG_FILE` would pollute or alter output (especially outside
    Bazel actions in stages 2 and 3). Keep `ReadFiles`'s guards rejecting bad
    `dir` paths and `*.tfvars[.json]` files
    (`TestReadFiles_RejectsBadArguments`): when `dir` is `""`, the staged
    directory is `tofu`'s working directory. Staging under `<dir>` also makes
    diagnostics name `<dir>/<file>`.
  - Don't pass `vars` or `var_files` to `TofuModules`. Variables in a `module`
    `source` take their defaults there; if `vars` or `var_files` evaluate a
    different address at `tofu init`, OpenTofu fails with
    `"Module source has changed"`.
  - Don't parse `tofu`'s diagnostics (`tofushow.LoadError`; returned only when
    `tofu` exits non-zero with non-empty stderr, not on signals or empty
    stderr).
  - `tofu show -json -module` (added in OpenTofu 1.11) carries no
    `format_version`, and OpenTofu's v1 compatibility promises list `show -json`
    without naming `-module`. Keep `tofushow.Parse`'s checks (`root_module`,
    call `source`, `provider_config` `name` and `full_name`, one `full_name`
    per local name, and agreeing `version_constraint` values per `full_name`),
    `//tf/private/cmd/internal/tofushow:tofushow_test` (`TestReadFiles_Facts`,
    `TestReadFiles_LocalNames`, `TestParse`), and per-version coverage in
    `module_dependency_<version>_test` (`//reader` and `//reader:providers` in
    `tests/integration/module_dependency_test.sh` step 9).
- Parse module `source` strings only with `tfconfig.ParseSource` in Go
  (`modscan parse`), matching OpenTofu's `addrs.ParseModuleSource` via
  `registry-address/v2` and hand-written git and archive parsers.
- Don't add `go-getter`: its root package pulls the S3 and GCS SDKs into every
  downstream host-tools build.
- Leave `version` constraint checking against `module_version` to `tofu`, which
  checks each call against the manifest during `tofu init -get=false`.
- Match module calls to declarations with `tfconfig.BestMatch` in `modscan`
  and the manifest tool, never in Starlark or by raw string
  comparison. The match leaving the shortest `//<subdir>` wins, so a
  submodule's own declaration beats its parent's regardless of spelling (issue
  #97).
- Allow a module to call its own address with `//<subdir>` (issue #95):
  - Pass `--self` to `modscan` so self-called directories join the served
    fixpoint without adding a self-dep.
  - Have a `tf_library` with `module_source` pass itself to `modules_manifest`
    as `own`, never `direct`, because direct deps must be called
    (`module_dep_unused`) whereas a module needn't call itself.
  - Mark `own` as `callable` in `modules_manifest` so the library can call its
    `module_source` with `//<subdir>`. Calls inside a self-called submodule
    still count toward `module_dep_unused` because the submodule's files are in
    the library's `srcs`.
  - Cross-calls between two tags over one repository form a Bazel dependency
    cycle and are not supported.
- Materialise external-repository files under `external/<canonical repo>/`
  (`work_tree_path`), and include `label.workspace_root` in rule paths so
  external libraries build standalone.
- Reject relative `source` paths that cross repository boundaries, because
  canonical repository names belong to Bazel.
- In `tf_modules` (`tf/modules/extensions.bzl`; `README.md`, "Modules"):
  - Generate `:_rules_tofu_module` aliased at `@tf_modules//<path>`, and keep
    per-module repositories private.
  - Keep two repositories per module (issue #157; `tf_modules_test.sh`, steps
    9 and 10):
    - `_tf_module_files` (`files_repo_name`, `tf_module_files_<name>`) fetches
      and tidies `tree/`, writes `metadata.json` and an empty `BUILD.bazel`,
      and takes only `source` and the module's pin.
    - `_tf_module_repo` (`repo_name`, `tf_modules_<name>`) reads
      `metadata.json` via `files` so changes to that file re-trigger it, runs
      `_check_directories` and `modscan`, symlinks `tree/` in (`_link_files`)
      and writes the `BUILD` files.
    Keep `modules` (label to `source`), `subdir` and `subdirs` off
    `_tf_module_files`. Attributes are part of Bazel's rule key and Bazel 9's
    contents-cache key, so putting them on `_tf_module_files` would re-fetch a
    module when its `subdirs` changed or when another module was declared,
    removed or changed `source`. Regenerating `_tf_module_repo` reads local
    files and downloads nothing.
  - Keep `pin` in `metadata.json` even though nothing reads it
    (`tf_modules_test.sh`, step 10). `_tf_module_repo` has no pin attributes
    and Bazel does not track the files `modscan` reads. Without `pin`, a new
    commit or archive that edits only existing files leaves `metadata.json`
    unchanged and the library's `BUILD` files stale.
  - Record `git_submodules` and removed `directory_symlinks`
    (`_check_directories`) in `metadata.json` using paths relative to `tree/`
    only (`"."` for the root, `null` outside `tree/`), never host paths. The
    contents cache shares `_tf_module_files` across output bases.
  - Keep the root and each directory down to `subdir` real directories in
    `_link_files`. `_tf_module_repo` writes `BUILD.bazel` at the root and in
    `subdir`. Writing one through a symlinked directory would land in the shared
    `_tf_module_files` repository.
  - When `subdir` is non-empty, generate a root `//:_rules_tofu_files`
    filegroup and add it to `data` so relative paths leading outside `subdir`
    resolve.
  - List as `srcs` the configuration files of the directories `modscan`
    reports in `dirs`, and exclude every configuration file from the `data`
    globs (`_CONFIG_GLOBS`), so an uncalled example never reaches a work tree
    (issue #140). Files that a local path reaches outside `subdir` are in the
    root package, which exports them.
  - Keep `allow_empty = True` on both generated globs (`data` and
    `//:_rules_tofu_files`). A directory holding only configuration files
    leaves the glob empty (issue #164).
  - Read `subdirs` on `tf_modules.module` (`modscan --subdir`) for
    `//<subdir>` directories outside `modules/` that the module does not call
    itself. Combine the tags that declare the module, for a registry module
    only those at the selected version (`_subdirs`), so the root can drop a
    directory a newer version lacks. The generated library includes their
    configuration and providers (issue #76).
  - List dot-prefixed files with a configuration extension as `data` by name
    (`_dot_files`). tofu ignores them, `glob` cannot tell them from
    configuration, and `check_no_config_in_data` lets them through.
  - Keep the `_rules_tofu_` prefix on both target names so they never collide
    with upstream files named `files` or `module` (issue #61).
- Reach submodules via `//<subdir>` rather than separate hub targets: listing
  submodule targets on the hub would require fetching every module up front.
- Pin every `tf_modules` fetch so bumping a module updates `MODULE.bazel.lock`
  (`bazel mod deps --lockfile_mode=update`):
  - Resolve git `?ref=` via `git ls-remote` into a commit fact; never fetch by
    moving tag.
  - Record registry modules as `<path>@<version>` facts holding the download
    location and commit or digest.
  - Require `sha256` on archive modules.
- Tidy and scan fetched module trees (`_tidy`; `README.md`, "From a registry,
  git or an archive"):
  - Delete `_BAZEL_FILES` and every symlink except in-tree file symlinks before
    running `modscan`. `_BAZEL_FILES` includes `.bazelignore`, which Bazel
    honours at any repository root (issue #102).
  - Walk fetched trees with `walk` in `tf/private/fetched_tree.bzl`, never
    `is_dir` alone. `is_dir` is true on directory symlinks, where ancestor
    links loop and fail `glob` (issue #61).
  - Fail the fetch in `modscan` when a module calls an undeclared address,
    naming the tag to add.
- Don't fetch git submodules (`README.md`, "Known gaps"): they can't be tested
  offline and pull from commit-chosen URLs. `_tf_module_files` lists gitlinks
  and `modscan --git-submodule` rejects modules inside or calling into one,
  naming the submodule (issue #94).
- Keep the non-dev `use_extension` for `@tf_providers` in `rules_tofu`'s
  `MODULE.bazel` so libraries generated by `tf_modules` can reference
  `@tf_providers//...`: a repository generated by an extension sees only the
  repositories visible to the module that defines the extension.
- Don't check walked modules' providers in the manifest action, and don't parse
  `tofu`'s error output to suggest a label (issue #144):
  - Every configuration file is in some target's `srcs`
    (`check_no_config_in_data`, issue #140) and checked in dependency order
    (issue #143). Strict deps below (issues #135 and #153) checks every
    provider OpenTofu reports for `srcs`, whether in `required_providers`
    (`missing_provider`) or implied by a resource, data source or `provider`
    block (`implied_provider`), naming the directory and
    `@tf_providers//<ns>/<name>`.
  - A provider can still be missing at `tofu init` in `TofuValidate`, for
    example in an empty-root `tf_test` whose `run` block loads a module that
    needs a provider (`empty_root_test_provider`) or when the lock no longer
    satisfies a version constraint (`stale_lock`). `TofuValidate` runs
    `tofu init` only in a target's root module directory, so any `tofu init` or
    `tofu validate` failure in files of a subdirectory that nothing in that
    target calls (a bundle, `modules/*` or `subdirs`) appears in `TofuValidate`
    of the first target whose root module calls them. OpenTofu's error text can
    change between releases.
- Enforce strict deps in the manifest action with no opt-out (issues #135 and
  #153; `README.md`, "Strict dependencies"; `strict_deps_*` cases):
  - Stage and read a target's configuration `own_srcs` one directory at a time
    with `tofushow.Reader.ReadFiles`, without any other target's files in that
    directory, and check every provider local name (`m.LocalNames`, including
    implied ones, skipping `isBuiltin`) and call (`m.Calls`) OpenTofu reports
    (`TestBuildManifest_StrictDeps`). Never attribute requirements or calls by
    package, whole work-tree directory or HCL source range, and do not inherit
    direct declarations across targets.
  - Resolve each provider local name in `m.LocalNames` against the whole
    work-tree directory (`readDir(dir).LocalNames`, the same cached read the
    walk uses) before checking `--direct-provider`
    (`TestBuildManifest_StrictDeps`, `TestReadFiles_LocalNames`). This is the
    one deliberate exception to reading a target's files alone: a `tf_deploy`
    configuring a non-`hashicorp` provider (e.g. `provider "posthog" {}`) whose
    `required_providers` sits in a same-package `tf_library` in `deps` would
    otherwise report `hashicorp/<name>`, and OpenTofu rejects a second
    `required_providers` entry in the directory. Calls and which local names
    are used still come from the target's files alone.
  - Require each target's files in a directory to stand alone (issue #153;
    `strict_deps_override` in `tests/integration/BUILD.bazel`,
    `TestBuildManifest_StrictDeps`): an override file (`tfconfig.IsOverride`)
    must be in the same target as the block it overrides, a `module` `source`
    referring to a local value or variable needs that declaration in the same
    target, and a `.tofu` or `.tofu.json` file in another target does not
    shadow this target's `.tf` or `.tf.json` file during strict deps.
    `standAloneHint` appends a hint only on `*tofushow.LoadError` when the
    staged files include an override file, without parsing `tofu`'s output.
  - Check only a target's `srcs`, plus `run` blocks in a `tf_test`'s `srcs`,
    against `--direct-provider`, `callable` modules and `dep_dirs`. Do
    not re-check files from `deps` in the root module's directory
    (`examples/aws`), `module`'s configuration on a `tf_test`, or test files
    that came with `module`.
  - Allow relative calls from checked files only into directories containing the
    target's files or a direct dep's files (`TfLibraryInfo.dirs`,
    `TfDeployInfo.dirs`, `dep_dirs`). A second library in a direct dep's package
    or a directory supplied only by a transitive dep needs its own entry in
    `deps`.
  - Keep resolving every walked module call against transitive `modules`.
    Transitive `providers` still feed the plugin tree and lock; walked modules'
    providers are not checked.
  - Mark a direct `module_source` dep used on a relative call only when the call
    targets one of `module.dirs`, never via `within` on its package directory.
    The workspace root package (`""`) contains every directory.
  - Format `tf_modules` targets as `@tf_modules//...` in strict-deps errors
    (`hubLabel`) rather than their private canonical labels. Derive the hub path
    from the registry address or from the `+tf_modules_<name>` repository name
    (`repo_name`, `check_name`).
  - Match calls against all modules with `tfconfig.BestMatch` before checking
    `callable`, so strict deps never changes which library a call resolves to.
- Pin every registry provider used by the examples to an exact version in
  `examples/lock/versions.tf`, and use `>=` floors in `examples/aws`,
  `examples/gcp` and `examples/posthog`. Dependabot reads `/examples/lock`
  alone, so a provider required only elsewhere (such as by
  `examples/modules/stack`'s fetched module) would be dropped from the lock,
  and an exact pin elsewhere would fail once Dependabot bumps `examples/lock`.
  `examples/off_registry` is the sole `tf_providers.provider` example.
- Keep `examples/modules/{label,stack,git}` on lightweight providers alongside
  `//tests/integration:tf_modules_test` so CI exercises real upstream registry
  and git modules without ~170 MB cloud-provider downloads.
- Record `run { module { source = ... } }` blocks from `tf_test` files in the
  manifest (`--test-dir`):
  - Discover test files and calls with `tfconfig.TestFiles` and
    `ReadTestCalls`, where `.tofutest` shadows `.tftest`, and resolve local
    sources from the root module directory.
  - Format manifest keys with `TestRunKey` as
    `test.<file path with dots>.<run>`. `tofu` strips only `.tftest.hcl`,
    keeping `.tofutest.hcl`, `.tftest.json` and `.tofutest.json` in the key
    (issue #84).

### Extensions and the toolchain

- Return `module_ctx.extension_metadata` in all three extensions with dev vs
  non-dev deps split on `root_module_has_non_dev_dependency`, listing only the
  hubs (`tofu_toolchains`, `tf_providers`, `tf_modules`) so `bazel mod tidy`
  leaves `MODULE.bazel` unchanged.
- Arbitrate `tofu.version` from the root module: multiple root tags fail, one
  root tag wins, agreeing non-root tags win, disagreeing non-root tags fail
  naming the modules, and zero tags select `DEFAULT_VERSION`. `tofu` is one
  global binary for the build, so conflicting non-root requests cannot be
  resolved by picking the highest.
- Keep that version-resolution helper pure in
  `toolchain/version_resolution.bzl` returning `struct(version, error)`
  (`//toolchain/tests`) with `fail()` in the extension; don't inline it or make
  it last-write-wins.
- Download prebuilt OpenTofu binaries rather than building from source
  (`versions.bzl`; `README.md`, "Non-goals"): the pinned download is already
  hermetic and cached, whereas a source build would pull OpenTofu's ~300 Go
  modules into every downstream `go_deps` graph where `rules_tofu`, as a
  non-root module, could not apply OpenTofu's `replace` on `hashicorp/hcl`.
  Don't add a from-source TODO.
- Let digests enter `toolchain/versions.bzl` only through
  `bazel run //tools:update_versions` after `cosign` verification of
  `SHA256SUMS` (`CONTRIBUTING.md`, "Pinning a new OpenTofu release"); don't
  paste digests in by hand.
- Root modules needing a custom binary register `opentofu_toolchain`
  (`README.md`, "The `tofu` binary";
  `//tests/integration:custom_toolchain_test`).
- Supporting the `terraform` binary is a non-goal, not a feature to add on
  request without redesigning `tf/providers/source.bzl`.
- Keep `bazel_compatibility = [">=8.5.0"]`: Bazel 8.5 added `module_ctx.facts`,
  used unconditionally in `tf/modules/extensions.bzl` without a `hasattr`
  fallback. Raise `bazel_compatibility`, the CI matrix and `README.md`
  together. CI tests pinned `.bazelversion` and `8.5.0`, checking clean git
  status only on pinned jobs because `8.5.0` rewrites `MODULE.bazel.lock`.
- Run nested integration-test workspaces on the outer Bazel's version:
  `bazel_binaries.download` reads `@running_bazel`
  (`tests/integration/running_bazel.bzl`, `native.bazel_version`), so
  `USE_BAZEL_VERSION=8.5.0` tests nested builds on 8.5.0 too (issue #88).
  `nested_bazel.sh` verifies the version against `RUNNING_BAZEL_VERSION` in
  `env`. `MODULE.bazel` can load `running_bazel.bzl` despite `tests` being
  excluded from release archives because its sole caller is a `dev_dependency`.

### Rules and the runner

- Validate `tf_library` inline via `DefaultInfo.files` so
  `bazel build :foo_lib` is the validation contract. Don't add `:foo.validate`
  or `validate_test`.
- Generate no format targets (issue #155; `README.md`, "Formatting"). Don't
  bring back `.fmt`, `.fmt_check` or `fmt_test`: `tofu fmt -recursive` through
  `@rules_tofu//tofu` formats a tree by itself, and the rules added only
  scoping to `srcs`. `tf_library` and `tf_test` generate `:name` alone;
  `tf_deploy` adds only `.plan`, `.apply` and `.destroy`. CI's `lint` job runs
  `//tools:format.check` across tracked `.tf` and `.tfvars` files, plus
  `tofu fmt -check` on `examples` and `e2e/smoke` for the file kinds
  `rules_lint` skips. `module_dependency_<version>_test` runs the README's
  formatting commands.
- Check targets in dependency order (`README.md`, "How it works";
  `CONTRIBUTING.md`, invariant 2; issue #143; `dependency_invalid*`,
  `test_*_invalid` and `launcher_*` cases):
  - Carry check outputs in `TfLibraryInfo.validate_stamp`,
    `TfLibraryInfo.modules_manifest` and `TfDeployInfo.stamps`, and pass direct
    dependencies' outputs to `tf_init_validate` (`required_stamps`) and
    `modules_manifest` (`infos`). Direct dependencies suffice because each
    output already depends on its upstream outputs.
  - Pass manifests rather than validate stamps to `modules_manifest` so
    strict-deps checks do not wait for upstream `tofu init`.
  - Include `var_files_check_stamp` in `required_stamps` on `tf_deploy`
    validation (issue #150).
  - Don't use Bazel's `_validation` output group: validation actions run in
    parallel with downstream actions and tests rather than gating them, and
    `--norun_validations` skips them.
  - Put `TfDeployInfo.stamps` in both `runfiles` and `DefaultInfo.files` of
    `tf_runner`, and a `tf_test`'s own `validate_stamp` in its `runfiles`.
    `ctx.actions.write` takes no inputs; `bazel run` and `bazel test` build the
    executable and `runfiles`, while `bazel build` builds `DefaultInfo.files`
    for direct script execution. In `DefaultInfo.files` alone on `tf_test`,
    validation ran in parallel with `tofu test`.
  - Validate `tf_modules` libraries like any other, with no opt-out. Set
    `tf_modules_label` (`display_label`) for `TofuValidate` and `TofuModules`
    progress messages and `--label` so failures report `@tf_modules//<path>`
    instead of `:_rules_tofu_module` (`tf_modules_test` step 11).
- Keep `tf_deploy`'s `DefaultInfo.files` to its stamps (`TfDeployInfo.stamps`;
  issue #156; `run_directory_test` step 4). Including `work_tree_files` there
  made remote-cached builds download whole provider packages (~170 MB) for
  every top-level deploy; `work_tree_files` still feeds validation and
  `TfDeployInfo.work_tree_files` feeds launchers.
- Keep `tofu test` in the separate `tf_test` rule (`tf/private/test.bzl`;
  `README.md`, "Tests") with its own `deps` and `providers`.
- Allow empty root modules in `tf_library` and `tf_test` (issue #119; `empty`
  packages in `module_dependency` and `lock_update`):
  - Ignore `fs.ErrNotExist` on the root walk in the manifest tool. In
    `prepare` (`rundir.go`), skip `linkTree` when `--work-tree` does not exist
    and call `os.MkdirAll` on the run directory.
  - Copy `--lock-file` to `.terraform.lock.hcl` only when `hasConfig` is true
    on the run directory. In an empty root `tofu init` installs no providers
    and leaves the lock unpruned against an empty `.terraform/providers`.
  - Keep `runValidate`'s missing-provider guidance when an empty-root `tf_test`
    fails validation (`empty_root_test_provider`).
  - Don't make `tfconfig` treat missing directories as empty: `modscan` relies
    on the error.
- Reject an empty root in `tf_deploy` at analysis (`check_has_configuration`;
  `deploy_deps_only`, `deploy_subdirectory_only`), because `tofu` validates an
  empty directory and then refuses to plan it.
- Keep `tfconfig.HasConfig`, `hasConfig` in the runner and
  `check_has_configuration` aligned on what counts as configuration: regular
  non-dot files with the four config extensions. `hasConfig` duplicates
  `tfconfig.HasConfig` so `runner` links no HCL parser.
  `tfconfig.Effective` and `EffectiveTests` drop dotfiles for all callers; `pin`
  reads through `Effective`.
- Reject `TEST_SRC_EXTS` in `tf_library` and `tf_deploy` `srcs`
  (`check_no_test_files`; `test_file_in_srcs`). `ALLOWED_SRC_EXTS` admits
  `.hcl`, and `tofu` would otherwise load library test files in dependants.
  `tf_modules` excludes test files from `data` too (`_TEST_GLOBS`), because
  validating a fetched module at its root makes `tofu` read tests in its root
  and `tests/` directories, whose `run` blocks may load excluded examples.
- Reject configuration files in every rule's `data` (`check_no_config_in_data`;
  `config_in_data`; issue #140). `data` is not code: only `srcs` are checked
  against `providers` and `deps`. Don't go back to
  checking `data` only when a directory is loaded.
- Reject auto-loaded variable files (`terraform.tfvars[.json]`,
  `*.auto.tfvars[.json]`) in every rule's `data` (`check_no_auto_tfvars`;
  `auto_tfvars_in_data`). Any library directory is a root when validated or
  tested by `tf_test`, where auto-loaded files bypass duplicate-key checks
  (issue #93). Keep `_EXCLUDED` in `tf_modules`, which strips those files from
  upstream modules, in step.
- Pass `vars` as `-var` and `var_files` as `-var-file` (issue #150;
  `README.md`, "Variables and data files"; `variableArgs`, `var_flags`;
  `run_directory_test` step 5):
  - Order the flags as the deploy's `-var` sorted by name, the deploy's
    `-var-file`, a `tf_test`'s `-var-file`, then trailing arguments after `--`.
    Test `var_files` and arguments after `--` then override the deploy because
    later flags win in OpenTofu.
  - Pass `-var` and `-var-file` to `tofu init` and `tofu validate` as well as
    to the command. Forward trailing arguments after `--` to the command only.
    `TofuValidate` then checks `var_files` syntax on `tf_test`, where
    `dupcheck` does not run (`malformed_test_var_file`).
  - Don't generate a variables file from `vars`. OpenTofu named the old
    `rules_tofu.auto.tfvars.json` in diagnostics. Its precedence and reserved
    filename were `rules_tofu` details users had to learn.
  - Keep `vars` an `attr.string_dict` whose values pass verbatim after
    `-var=<name>=`. Don't encode Starlark values into HCL in the rules.
    OpenTofu parses each value by the variable's declared type.
  - Quote every interpolated value in generated launchers with `shell_quote`
    (`runner.bzl`, `test.bzl`). Values in `vars` may contain spaces, quotes,
    `$` and newlines.
  - Reject an empty `vars` key or one containing `=` at analysis in `var_flags`
    (`vars_name_with_equals`). `tofu` splits `-var` at the first `=` and would
    assign to the prefix before `=`.
  - When `--var` is passed, check in `TofuValidate` via
    `tofu show -json -module=.` after `tofu validate` that the root module
    declares every name (`checkVarsDeclared`; `vars_undeclared`).
    `tofu validate` accepts an undeclared `-var` while `tofu plan` rejects it.
    Asking `tofu` leaves configuration parsing to OpenTofu rather than
    `runner`. Don't check `var_files` keys because `tofu` only warns about
    undeclared names in a var file.
- Read a `tf_test`'s root module directory from `TfLibraryInfo.package_dir` or
  `TfDeployInfo.package_dir`, never `ctx.attr.module.label`. That allows tests
  to live in a `tests/` sub-package and works when `module` is an `alias`
  (issue #87; `examples/hello/tests:greet_alias_test`).
- Require all `tf_test` `srcs` to share one directory at or below that root
  (`test_dirs_split`), because `tofu test` reads the root directory and at most
  one test directory. When `srcs` sit in a subdirectory, pass `-test-directory`
  to `tofu init -get=false`, validate and test alike; omit it when they sit in
  the root directory, which `tofu` reads anyway.
- Prepare the run directory `<base>/work/<package-dir>` for every command with
  `prepare` and `(*runDir).init` (`tf/private/cmd/runner/rundir.go`; issue
  #148). Don't split a separate copy or staging path for `test` or `validate`:
  - Make every path flag (`--tofu`, `--work-tree`, `--plugin-dir`,
    `--modules-manifest`, `--lock-file`, `--stamp`, `--state-dir`,
    `--output-root` and `--var-file` via `varFilePtrs`) absolute in `run()`
    before dispatch (`TestRunner_RunDir_RelativeFlags`,
    `TestRunner_Test_RelativeFlags`). `tofu` runs in the run directory rather
    than the runner's working directory. `TofuValidate` receives
    exec-root-relative paths including a bare filename for a root-package
    custom `--tofu` binary. A `tf_test` launcher run directly by a relative
    path resolves `$0.runfiles` as a relative path
    (`module_dependency_<version>_test` step 5). Relative `--work-tree` paths
    would also produce dangling symlinks in `linkTree`.
  - Pass `--var-file="$R"/<work_tree_root>/<path>` in `tf_test`'s launcher
    (`tf/private/test.bzl`), matching `tf_runner` (`tf/private/runner.bzl`).
  - Keep `--command=test` (`runTest`, `test.go`) and `--command=validate`
    (`runValidate`, `validate.go`) free of `--state-dir`, `--output-root` and
    `checkStateDurable`. Pass `-backend=false` to `tofu init` on both because
    neither uses a backend and plain `tofu init` contacts configured backends.
  - In `runTest`, pass `-no-color` to `tofu init` and `tofu test`. Make
    `TEST_UNDECLARED_OUTPUTS_DIR` absolute in `runTest` and `base` absolute in
    `prepare` because `TMPDIR`, `TEST_TMPDIR` and
    `TEST_UNDECLARED_OUTPUTS_DIR` may be relative while `tofu` runs in the run
    directory. Fail if `errored_test.tfstate` exists after `tofu test` because
    OpenTofu writes it and still exits 0 when cleanup fails to destroy a
    resource. Copy `errored_test.tfstate` to `TEST_UNDECLARED_OUTPUTS_DIR`
    when set. Create `base` with `os.MkdirTemp` under `$TEST_TMPDIR` and leave
    it for Bazel to remove. When `TEST_TMPDIR` is unset, create `base` under
    `$TMPDIR` and remove it on exit unless `TEST_UNDECLARED_OUTPUTS_DIR` is
    also unset and `errored_test.tfstate` was written. Keeping `base` in both
    cases preserves the `errored_test.tfstate` path named in the failure
    message (`TestRunner_Test_ErroredStateKeptWithoutOutputs`).
  - In `runValidate`, create `base` with `os.MkdirTemp` under `$TMPDIR` and
    always remove it on exit because only `linux-sandbox` gives each action a
    fresh `TMPDIR` (issue #90). Discard `tofu init` stdout while keeping
    stderr. Keep the failure hints covered by `stale_lock`,
    `module_version_mismatch`, `empty_root_test_provider` and
    `tf_modules_test`.
- Write `tf_test` JSON logs via `-json-into`, which sets the minimum OpenTofu
  version to 1.12.0 (`README.md`, "Quick start" and "Bringing your own binary";
  `toolchain/versions.bzl`; 1.8.5 was dropped in issue #125). Don't pin older
  releases, probe flags, or fall back to `-json`, which replaces human output.
- If a newer flag raises the OpenTofu floor, update `toolchain/`
  (`versions.bzl` and `opentofu_toolchain` in `toolchain.bzl`), `README.md` and
  this file together, and keep a `tf_test` in
  `module_dependency_<version>_test`. Don't check `opentofu_toolchain.version`,
  because `custom_toolchain` sets `0.0.0`.
- Mirror `--work-tree` in `linkTree` as real directories and file symlinks
  (`TestRunner_Test_RunsScriptThroughLink`, `//provisioner:test` in
  `module_dependency_<version>_test`):
  - Symlinks inherit their target's executable mode automatically.
    `path.module` and `dirname "$0"` resolve inside `<base>/work`, so files
    created beside a script land there rather than in `<name>.work` or
    runfiles.
  - Never modify a linked file in place or write through its resolved target.
    Call `os.Remove` before writing in `installLockFile` and
    `installModulesManifest`. In `linux-sandbox`, writing through a link fails
    with `Read-only file system`. Without a sandbox (`--spawn_strategy=local`,
    `no-sandbox` or `local` tags, `bazel run` or a direct launcher run), the
    same write modifies the source file in the checkout.
- Reject directory symlinks in `linkTree` (`errDirectoryLink`, issue #89).
  `WalkDir` does not descend into them, manual walks loop on ancestor links,
  and a linked directory would let `tofu` write into `--work-tree`.
- Build a plugin tree for every `tf_test` even with `mock_provider`, because
  `tofu` still requires `.terraform/providers` for schemas.
- Gate live infrastructure in `tf_test` with Bazel tags rather than parsing
  `command = apply`: apply `block-network` by default, or `external` and
  `requires-network` when `allow_network = True`. `command = apply` is also the
  default for offline runs using `mock_provider` or local providers, which
  should stay sandboxed and cached, while `external` keeps live tests uncached.
- Set `cfg = "exec"` on `providers` in `tf_library`, `tf_deploy` and `tf_test`
  so `@tf_providers//<ns>/<name>` selects for the execution platform in both
  validation and launchers (`README.md`, "Known gaps"); don't design around
  cross-compilation. `TfProviderInfo` carries `platform`, and
  `materialize_plugin_tree` fails if any two providers in a plugin tree have
  different platforms, even at distinct addresses
  (`conflicting_provider_platforms`, `conflicting_provider_platforms_distinct`):
  a target and its `deps` resolve execution platforms independently, while one
  plugin tree serves a single `tofu` binary. Don't re-add `platform_key` to
  `opentofu_toolchain` or `TofuInfo` (issue #146).
- Constrain `tofu` toolchains with `exec_compatible_with` only (no
  `target_compatible_with`) so non-host `--platforms` flags still resolve one
  (issue #105), and keep rule Go tools at `cfg = "exec"`.
- Keep `--plugin-dir` mandatory on the runner. When no providers are in scope
  and `--plugin-dir` does not exist, fall back in `prepare` to an empty
  `<base>/plugins` directory (`TestRunner_MissingPluginDirFallsBack`,
  `TestRunner_Test_MissingPluginDirFallsBack`). Never `os.MkdirAll`
  `--plugin-dir` in runfiles or the exec root.
- Refuse `apply` and `destroy` unless state is durable or
  `allow_ephemeral_state` is set on `tf_deploy` (`README.md`, "State and
  backends"); don't add a second rule beside it. State under `bazel-bin` is
  wiped by `bazel clean` and forks across build configurations and checkouts,
  so applying against it orphans live infrastructure:
  - Count state as durable only with a remote backend, a `cloud` block, or a
    `backend "local"` whose `path` and `workspace_dir` are both absolute paths
    outside `--output-root` (`bazel-out`), resolving symlinks and allowing a
    missing tail (`localBackendEphemeral`). Null or empty local attributes use
    OpenTofu's relative defaults and are ephemeral, as is having no `backend`
    or `cloud` block.
  - Read the backend from the `.terraform/terraform.tfstate` record written by
    `tofu init` in the run directory (`recordedBackend`; issue #149), never by
    parsing configuration in the runner. Missing or empty `backend` metadata
    means no `backend` or `cloud` block; a `cloud` block has type `"cloud"`.
    Reading the record uses OpenTofu's own result after merging `_override`
    files and evaluating variables. Because the format is internal to OpenTofu,
    `module_dependency_<version>_test` covers it across `KNOWN_VERSIONS`.
  - Run the check in the Go runner (`checkStateDurable`) after `tofu init` and
    before `tofu apply` or `tofu destroy`, refuse when the record cannot be
    parsed, leave `plan` ungated, and don't weaken the refusal to a warning.
- Share `<state-dir>/work/<package-dir>` across `.plan`, `.apply` and
  `.destroy` in `runDeploy` via `prepare(*stateDir)` (`rundir.go`; issues #86,
  #91):
  - Never run `tofu` in per-target runfiles or the `<name>.work` build output.
    Runfiles are per-target and pruned by Bazel, so `.plan`, `.apply` and
    `.destroy` would resolve relative paths to different trees. `<name>.work`
    is an action input that must never gain `tofu`'s files or a
    `.terraform.lock.hcl`.
  - On each run, `clearRunDir` removes symlinks outside `<cwd>/.terraform`, the
    old `.terraform.lock.hcl`, `.terraform/providers` and
    `.terraform/terraform.tfstate` while keeping the rest of `.terraform/` and
    local state. `linkTree` errors rather than overwriting a regular file at a
    work-tree path so it cannot shadow configuration.
  - Don't add an up-front check that `--package-dir` exists in `--work-tree`.
    `check_has_configuration` already rejects an empty deploy at analysis.
  - Never rewrite a `backend` block or pass `-backend-config`, `-state` or
    `-state-out`. OpenTofu owns path and expression resolution across all
    backends. Without a `backend` or `cloud` block it writes
    `terraform.tfstate` in the run directory, so `tofu workspace` works there
    too. There is no run-directory lock (`README.md`, "Restrictions").
  - Pass `-var` and `-var-file` to `tofu init` as well as to the command
    because `backend` and `cloud` blocks may reference variables.
- Keep deploy launchers (`tf/private/runner.bzl`) standalone like a
  `go_binary`, with each `tf_runner` in its deploy's package:
  - Resolve runfiles and state from `$0` with absolute paths so `bazel run` and
    direct invocation match.
  - Never read `BUILD_WORKSPACE_DIRECTORY` in launchers or the runner; only
    `pin` may read it.
- Keep `@rules_tofu//tofu` (`tf/private/tofu_binary.bzl`; issue #152) plain
  `tofu`: no staging, no `tofu init`, no added flags and the environment
  untouched:
  - Keep `@rules_tofu//tofu` the only launcher that reads
    `BUILD_WORKING_DIRECTORY`. Change directory only when it is set, and never
    fail when it is unset: `rules_lint`'s `format_test` runs it under
    `bazel test`, which does not set `BUILD_WORKING_DIRECTORY`.
  - Look for the binary in `RUNFILES_DIR` and `TEST_SRCDIR` before
    `$0.runfiles`, checking that the executable binary exists in each
    (`//tests/tofu:tool_test`). As the `terraform` tool of `rules_lint`'s
    `format_multirun`, the launcher is inside the other target's runfiles and
    `$0.runfiles` does not exist.
  - Keep the launcher as the target's only default output. `rules_lint` expands
    `$(rlocationpaths)` of its tool and expects one path.
  - Keep the rule under `tf/private/`, and keep `tofu/` free of dev-only
    dependencies. `tofu/` ships in the release archive, and `//tofu:all_files`
    is in `//:local_repository_files` so nested integration workspaces see it.
- Isolate `tofu`'s environment (`tofuEnv`, used by every command) and init
  flags in the runner:
  - Pin `TF_DATA_DIR` to `<cwd>/.terraform` so an inherited `TF_DATA_DIR`
    cannot make `tofu init -get=false` miss the module manifest.
  - Strip `TF_PLUGIN_CACHE_DIR` so `tofu` uses only the vendored plugin tree.
  - Remove `.terraform/terraform.tfstate` before `tofu init` (`clearRunDir`,
    `backendRecordPath`; issue #92) so saved backend settings cannot block an
    edited `backend` block until `bazel clean`. Don't pass `-reconfigure`:
    `tofu init` rejects `-reconfigure` when a `cloud` block is present, and
    removing a `backend` block under `-reconfigure` leaves the stale record in
    place for `recordedBackend` to read.
- Route all work-tree and plugin-tree materialisation through
  `tf/private/work_tree.bzl` (`materialize`, `materialize_plugin_tree`,
  `work_tree_root`, `plugin_tree_root`) across `library.bzl`, `deploy.bzl` and
  `test.bzl`. Keep `<name>.plugins/` a sibling of `<name>.work` so `prepare`
  does not walk and link provider packages into `<base>/work` and `-plugin-dir`
  reads them in place.

### Repository hygiene

- Leave `version` unset in `module()` in `MODULE.bazel` (`CONTRIBUTING.md`,
  "Releasing"), and use `X.Y.Z` with a registry link in docs.
- Keep comments out of `module(...)`: publish-to-bcr inserts `version` before
  the first `)` after `module(`, so a parenthesis in a comment corrupts the BCR
  patch. That broke the 0.1.1 release.
- Keep Starlark unit tests in `//toolchain/tests`, `//tf/providers/tests` and
  `//tf/modules/tests`, and never reference `dev_dependency` repos
  (`bazel_skylib`) from downstream-loaded `toolchain/BUILD.bazel`
  (`:toolchain_type`), `tf/providers/BUILD.bazel` or `tf/modules/BUILD.bazel`.
- Keep `dev_dependency` consumers out of the release archive (`CONTRIBUTING.md`,
  "Releasing"; `release_prep.sh`), adding explicit prefix-anchored exclusions
  for any `tests` subpackage under a shipped directory.
- Depend on `all_files` filegroups rather than `bzl_library` in `//docs`
  (`CONTRIBUTING.md`, invariant 3), because `bazel_skylib` is a
  `dev_dependency`.
- Keep the root, `e2e/smoke` and `tests/integration/*` `.bazelversion` files
  byte-identical (`cmp` in CI lint), and bump them all together. Nested modules
  are separate Bazelisk boundaries that never see the root pin.
- List the four `bazel-*` symlinks of every nested workspace in `.bazelignore`,
  which CI lint checks. Building inside a nested workspace creates symlinks
  back to the output base that would make root `//...` loop, and `.bazelignore`
  takes no globs.
- Regenerate `--deleted_packages` in `.bazelrc` via
  `bazel run @rules_bazel_integration_test//tools:update_deleted_packages`
  whenever nested packages change; don't edit those lines by hand.
- Run formatters through `//tools:format` and `//tools:format.check`
  (`CONTRIBUTING.md`, "Formatting and lint"), never
  `bazel run @buildifier_prebuilt//:buildifier -- -r .`: `bazel run` starts
  `buildifier` in its runfiles directory rather than the workspace, so `.`
  holds no source files and checks nothing.

## Branch / PR conventions

Development branch (`claude/...`); push there, not `main`. Don't open a
PR unless asked. Don't include the model identifier (`claude-opus-4-7`
etc.) in any commit message, PR title, or code artifact.
