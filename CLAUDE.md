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
  `//tf/modules/tests/...`) and `.fmt_check` targets need no Go toolchain.
- Everything else (`bazel build //...`, `tf_library`, `tf_deploy`, `tf_test`,
  Bazel Go tests, and workspaces using `from_lock` or `tf_modules`) needs
  `rules_go` to download the Go SDK for `//tf/private/cmd/modules`, `dupcheck`,
  `runner` and `@rules_tofu_host_tools`. That failed in the sandbox when
  `rules_go` fetched `https://go.dev/dl/?mode=json` (proxy 403); CI has open
  network and is unaffected.
- Outside Bazel, run Go unit tests with `go test ./tf/private/cmd/...` (in
  Bazel: `bazel test //tf/private/cmd/...`). Plain `go test` skips the `runner`
  and `dupcheck` tests and still reports `ok` unless `-runner=<path>` and
  `-dupcheck=<path>` are passed:
  - `go build -o /tmp/runner ./tf/private/cmd/runner && go test ./tf/private/cmd/runner -runner=/tmp/runner`
  - `go build -o /tmp/dupcheck ./tf/private/cmd/dupcheck && go test ./tf/private/cmd/dupcheck -dupcheck=/tmp/dupcheck`

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
  - Reject `.terraform.lock.hcl` in `srcs` or `data`, and don't relax the
    work-tree check in `init_action.bzl`. `ALLOWED_SRC_EXTS` in
    `tf/private/work_tree.bzl` admits `.hcl`, so that check is the backstop.
  - Pass the managed lock via `TfProviderInfo.lock` (`--lock-file` on the
    runner) and copy it into the work tree only after that check runs.
- Don't pass `-lockfile=readonly` to `tofu init`: it fails when the lock has
  entries a work tree doesn't use.
- Verify every provider download against a per-platform digest: the tag's
  `sha256`, a registry digest recorded as a `<source>@<version>` fact in
  `MODULE.bazel.lock`, or a registry digest vouched for by the lock's `zh:`
  hashes (`lock_sha256s`).
- Resolve a locked provider's digests in its repository rule
  (`_locked_downloads`), never in the extension and never as facts. The lock
  alone pins it, so a bot updating `versions.tf` and `.terraform.lock.hcl`
  leaves `MODULE.bazel.lock` valid (`//tests/integration:lock_update_test`).
- Set `reproducible = True` on all three extensions (`tofu`, `tf_providers`,
  `tf_modules`). Every repository they declare is determined by the module
  graph, watched lock file, `versions.bzl` table and recorded facts, so copying
  repo specs into `MODULE.bazel.lock` pins nothing extra, and returning only
  active facts lets stale entries drop out.
- Validate extension facts on read (`valid_cached_sha256s`, `valid_fact` and
  `_cached_registry`), treating any bad fact as a cache miss to re-resolve and
  never calling `fail()` (issue #96). `MODULE.bazel.lock` is a checked-in file
  that can outlive the code that wrote it or be mis-merged. `_cached_registry`
  rejects both a registry fact whose pin doesn't match its location and a
  well-formed fact stored under a key of the other kind.
- End download repository rules (`_tofu_download`, `_tf_provider_download`,
  `_tf_module_repo`) with `return reproducible(repository_ctx)`
  (`tf/private/repo_metadata.bzl`). Bazel 9's repository contents cache then
  shares fetches across output bases, so the nested integration tests fetch
  each provider, `tofu` binary and module once per machine. Declaring it is
  sound only while every download those rules make is checked against a digest
  carried by their attributes or the lock: the registry supplies URLs, never
  contents. Any rule that fetches anything unpinned must stop declaring it. The
  cheap hub repositories omit it.
- Silently ignore a dependency's `from_lock` (`README.md`, "Providers"): a
  dependency carries one from being developed as a root module of its own,
  while in a consumer workspace the root owns provider versions and libraries
  only constrain them.
- Always create the `@tf_providers` hub so dependencies' `use_repo` calls
  resolve even when the root declares no providers.
- Treat a missing lock file as empty so `pin` can write the first one.
- Turn an unusable lock entry into a `_tf_provider_download` with `error` set
  so it fails on fetch, never a `fail()` in the extension. Failing the
  extension would also break `@tf_providers//:pin`. Only a malformed lock file
  fails the extension: bad HCL, an invalid provider address, a duplicate
  provider block, or an entry missing `version`.
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
  - Pass `--output=streamed_jsonproto`, request `module_source` as the sole
    rule attribute, pass `--keep_going`, and accept exit 3 because `deps()`
    traverses `providers` into `@tf_providers` packages that don't exist yet.
  - Query every `_tf_library` in `deps`, not only those setting `module_source`
    (issue #60).
  - In external repositories, count files only in the directories
    `(*library).serves` and `tfconfig.ModuleDirs` return: `modscan`'s
    directories plus `//<subdir>` submodules resolved via `tfconfig.BestMatch`
    to a fixpoint. That keeps fetched modules' examples out of the lock
    (`tf_modules_test`).
  - Have an external library with neither own configuration nor `module_source`
    serve nothing so an aggregate of deps doesn't sweep in child `modules/`.
    Have one with `module_source` and no own configuration act as a bundle
    serving `modules/` and called `//<subdir>` paths (issue #123;
    `lock_update_test`). `tfconfig.ModuleDirs` therefore allows an empty module
    root, while `modscan` checks separately that a fetched module has root
    configuration.
  - Combine a directory's constraints by provider source in `requirements`,
    never by local name. Don't revert to `ReadRequiredProviders`, which assumes
    a single module: `bazel query` returns files rather than targets, and two
    targets in one package may share a local name (issue #99).
  - Generate a fresh scratch configuration with one child module per directory
    and only `required_providers`, then run `tofu providers lock` there while
    excluding root-tagged providers via `--exclude-source`. Extracting only
    `required_providers` avoids loading resources or module calls out of
    context, and `tofu` combines child-module constraints natively. Running in
    a fresh directory works around `tofu providers lock` having no `-upgrade`
    flag and otherwise keeping existing versions. When `--exclude-source`
    leaves no providers, write an empty header-only lock so moving a provider
    to a root tag clears the conflict.
  - Don't verify the lock's `constraints` attribute: Renovate and Dependabot
    rewrite it in their own form.
- Vendor the full unpacked provider zip (`package_files`) and symlink all of it
  in `materialize_plugin_tree`. Lock `h1:` hashes cover the whole package
  directory, so a binary-only plugin tree fails `tofu init`.
- Resolve provider zip URLs through the pure functions in
  `tf/providers/source.bzl` (`//tf/providers/tests`): `url_template` with a
  required `sha256`, otherwise the OpenTofu registry for every namespace
  including `hashicorp/`. OpenTofu canonicalises bare `<ns>/<name>` addresses
  to `registry.opentofu.org` and looks for plugins under that host, and
  `releases.hashicorp.com` builds have different checksums, so don't
  special-case it. Check the registry's `shasum` against a tag's pinned
  `sha256` before downloading (`registry_download_url`,
  `registry_sha256_mismatch`) so a wrong pin fails immediately naming the right
  digest.
- Lower-case all provider sources to match OpenTofu's canonical addresses.
- Keep per-provider `tf_providers_<ns>_<name>` repositories private: omit them
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
- Parse module `source` strings only with `tfconfig.ParseSource` in Go
  (`modscan parse`), matching OpenTofu's `addrs.ParseModuleSource` via
  `registry-address/v2` and hand-written git and archive parsers.
- Don't add `go-getter`: its root package pulls the S3 and GCS SDKs into every
  downstream host-tools build.
- Leave `version` constraint checking against `module_version` to `tofu`, which
  checks each call against the manifest during `tofu init -get=false`.
- Match module calls to declarations with `tfconfig.BestMatch` across
  `modscan`, the manifest tool and `pin`, never in Starlark or by raw string
  comparison. The match leaving the shortest `//<subdir>` wins, so a
  submodule's own declaration beats its parent's regardless of spelling (issue
  #97).
- Allow a module to call its own address with `//<subdir>` (issue #95):
  - Pass `--self` to `modscan` so self-called directories join the served
    fixpoint without adding a self-dep.
  - Have a `tf_library` with `module_source` pass itself to `modules_manifest`
    as `own`, never `direct`, because direct deps must be called
    (`module_dep_unused`) whereas a module needn't call itself.
  - Keep `childOwn` true when a `module_source` call lands back in the root's
    package so calls inside a self-called submodule still count as using the
    library's direct `deps`.
  - Cross-calls between two tags over one repository form a Bazel dependency
    cycle and are not supported.
- Materialise external-repository files under `external/<canonical repo>/`
  (`work_tree_path`), and include `label.workspace_root` in rule paths so
  external libraries build standalone.
- Reject relative `source` paths that cross repository boundaries, because
  canonical repository names belong to Bazel.
- In `tf_modules` (`tf/modules/extensions.bzl`; `README.md`, "Modules"),
  generate `:_rules_tofu_module` with `//:_rules_tofu_files` in `data`, aliased
  at `@tf_modules//<path>`, and keep per-module repositories private. Those
  target names avoid colliding with upstream files named `files` or `module`
  (issue #61).
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
  offline and pull from commit-chosen URLs. `_fetch` lists gitlinks and
  `modscan --git-submodule` rejects modules inside or calling into one, naming
  the submodule (issue #94).
- Keep the non-dev `use_extension` for `@tf_providers` in `rules_tofu`'s
  `MODULE.bazel` so libraries generated by `tf_modules` can reference
  `@tf_providers//...`: a repository generated by an extension sees only the
  repositories visible to the module that defines the extension.
- Check walked `required_providers` against `TfProviderInfo` (`--provider`) in
  the manifest action so a missing provider fails naming the
  `@tf_providers//<ns>/<name>` label to add before `tofu init` fails less
  helpfully (`missing_provider`, `tf_modules_test` step 3). Module fetches
  cannot see which `//<subdir>` submodules outside `modules/` callers will
  reach (issue #76), and providers cannot be added automatically there because
  provider labels must be dependencies before analysis.
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
  - Query `_tf_test` in `pin` for the `//<subdir>` fixpoint, while
    `tfconfig.ModuleDirs` ignores fetched modules' `tests/` directories brought
    in by `glob(["**"])`.

### Extensions and the toolchain

- Return `module_ctx.extension_metadata` in all three extensions with dev vs
  non-dev deps split on `root_module_has_non_dev_dependency`, listing only the
  hubs (`tofu_toolchains`, `tf_providers`, `tf_modules`) so `bazel mod tidy`
  leaves `MODULE.bazel` unchanged.
- Arbitrate `tofu.version` from the root module: multiple root tags fail, one
  root tag wins, agreeing non-root tags win, disagreeing non-root tags fail
  naming the modules, and zero tags select `DEFAULT_VERSION`. Unlike providers,
  `tofu` is one global binary for the build rather than a per-library
  dependency, so conflicting non-root requests cannot be resolved by picking
  the highest.
- Keep that version-resolution helper pure in
  `toolchain/version_resolution.bzl` returning `struct(version, error)`
  (`//toolchain/tests`) with `fail()` in the extension; don't inline it or make
  it last-write-wins.
- Resolve provider tags as "root wins, else `highest_version`"
  (`tf/providers/source.bzl`). Don't parse agreeing tags; when non-root tags
  differ, require plain numeric versions and return an error naming the modules
  on prereleases such as `2.0.0-rc1` vs `1.9.0` (issue #98), because choosing
  between a prerelease and a prior release is the root module's call.
- Defer that version-ordering error to `_tf_provider_download` via `error`
  rather than calling `fail()` in the extension, so a `from_lock` root can
  settle it with `pin` (`unorderable_versions`).
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
  used unconditionally in `tf/providers/extensions.bzl` without a `hasattr`
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
  or `validate_test`; the only generated test sub-target is `:foo.fmt_check`.
- Keep `tofu test` in the separate `tf_test` rule (`tf/private/test.bzl`;
  `README.md`, "Tests") with its own `deps` and `providers`.
- Allow empty root modules in `tf_library` and `tf_test` (issue #119; the
  `empty` packages in the fmt and lock_update fixtures):
  - Ignore `fs.ErrNotExist` on the root walk in the manifest tool, create
    `$CWD` before copying in the validate action, and call `MkdirAll` in the
    test runner.
  - Don't copy `.terraform.lock.hcl` into an empty root: `tofu init` installs
    no providers there and leaves the lock unpruned against an empty plugin
    cache.
  - Keep the validate script's guidance when an empty-root test fails for a
    missing provider (`empty_root_test_provider`).
  - Don't make `tfconfig` treat missing directories as empty: `modscan` and
    `pin` rely on the error.
- Reject an empty root in `tf_deploy` at analysis (`check_has_configuration`;
  `deploy_deps_only`, `deploy_subdirectory_only`), because `tofu` validates an
  empty directory and then refuses to plan it.
- Keep `HAS_CONFIG` (validate script), `tfconfig.HasConfig` and
  `check_has_configuration` aligned on what counts as configuration: regular
  non-dot files with the four config extensions. `tfconfig.Effective` and
  `EffectiveTests` drop dotfiles for all callers including `pin`.
- Reject `TEST_SRC_EXTS` in `tf_library` and `tf_deploy` `srcs`
  (`check_no_test_files`; `test_file_in_srcs`). `ALLOWED_SRC_EXTS` admits
  `.hcl`, and `tofu` would otherwise load library test files in dependants.
  Fetched modules' tests in `data` stay unchecked and inert unless loaded.
- Reject auto-loaded variable files (`terraform.tfvars[.json]`,
  `*.auto.tfvars[.json]`) in every rule's `data` (`check_no_auto_tfvars`;
  `auto_tfvars_in_data`). Any library directory is a root when validated or
  tested by `tf_test`, where auto-loaded files bypass duplicate-key checks and
  can override `vars` (issue #93). Keep `_EXCLUDED` in `tf_modules`, which
  strips upstream `terraform.tfvars` and `*.auto.tfvars` files, in step.
- Pass `var_files` explicitly with `-var-file`, rejecting only the reserved
  `rules_tofu.auto.tfvars.json` (`auto_tfvars_conflict`).
- Read a `tf_test`'s root module directory from `TfLibraryInfo.package_dir` or
  `TfDeployInfo.package_dir`, never `ctx.attr.module.label`. That allows tests
  to live in a `tests/` sub-package and works when `module` is an `alias`
  (issue #87; `examples/hello/tests:greet_alias_test`).
- Require all `tf_test` `srcs` to share one directory at or below that root,
  and pass `-test-directory` to `tofu init -get=false`, validate and test alike
  (`test_dirs_split`).
- Keep `--command=test` (`tf/private/cmd/runner/test.go`) separate from the
  deploy runner path and ephemeral-state gate:
  - Copy the work tree into `$TEST_TMPDIR` and take neither `--state-dir` nor
    `--output-root`.
  - Run `tofu init` with `-backend=false`: `tofu test` ignores backends, while
    plain `tofu init` contacts them.
  - Put any fallback empty plugin directory under `$TEST_TMPDIR`, never in
    runfiles.
  - Fail if `errored_test.tfstate` exists after `tofu test`: OpenTofu writes it
    and still exits 0 when cleanup fails to destroy a resource.
- Write `tf_test` JSON logs via `-json-into`, which sets the minimum OpenTofu
  version to 1.12.0 (`README.md`, "Quick start" and "Bringing your own binary";
  `toolchain/versions.bzl`; 1.8.5 was dropped in issue #125). Don't pin older
  releases, probe flags, or fall back to `-json`, which replaces human output.
- If a newer flag raises the OpenTofu floor, update `toolchain/`
  (`versions.bzl` and `opentofu_toolchain` in `toolchain.bzl`), `README.md` and
  this file together, and keep a `tf_test` in
  `module_dependency_<version>_test`. Don't check `opentofu_toolchain.version`,
  because `custom_toolchain` sets `0.0.0`.
- Preserve only the executable bit in `copyFile` (`0o755` or `0o644`, never
  `Perm()`): scripts run by `local-exec` provisioners or `external` data
  sources must stay executable, while Bazel outputs are read-only and `tofu`
  and subsequent copies write into the tree.
- Reject directory symlinks in both `copyTree` and `linkTree`
  (`errDirectoryLink`, issue #89): `WalkDir` doesn't descend into them, manual
  walks loop on ancestor links, and directory symlinks in `linkTree` would let
  `tofu` write into the work tree.
- Build a plugin tree for every `tf_test` even with `mock_provider`, because
  `tofu` still requires `.terraform/providers` for schemas.
- Gate live infrastructure in `tf_test` with Bazel tags rather than parsing
  `command = apply`: apply `block-network` by default, or `external` and
  `requires-network` when `allow_network = True`. `command = apply` is also the
  default for offline runs using `mock_provider` or local providers, which
  should stay sandboxed and cached, while `external` keeps live tests uncached.
- Select exec-platform provider binaries in both validation and the runner
  (`README.md`, "Known gaps"); don't design around cross-compilation.
- Constrain `tofu` toolchains with `exec_compatible_with` only (no
  `target_compatible_with`) so non-host `--platforms` flags still resolve one
  (issue #105), and keep rule Go tools at `cfg = "exec"`.
- Keep `--plugin-dir` mandatory on the runner and `os.MkdirAll` it for the
  zero-provider case.
- Refuse `apply` and `destroy` unless state is durable or
  `allow_ephemeral_state` is set on `tf_deploy` (`README.md`, "State and
  backends"); don't add a second rule beside it. State under `bazel-bin` is
  wiped by `bazel clean` and forks across build configurations and checkouts,
  so applying against it orphans live infrastructure:
  - Count state as durable only with a remote backend, a `cloud` block, or a
    `backend "local"` whose `path` and `workspace_dir` are literal absolute
    paths outside `--output-root` (`bazel-out`), resolving symlinks and
    allowing a missing tail (`localBackendEphemeral`). No backend block, or any
    other local one, is ephemeral.
  - Detect the backend with a read-only filesystem scan of the root module's
    configuration in the launcher's work tree (`detectBackend`).
  - Run the check in the Go runner before starting `tofu` or creating
    `--state-dir`, leave `plan` ungated, and don't weaken it to a warning.
- Run `tofu` for deploys in `<state-dir>/work/<package-dir>` (`rundir.go`), a
  symlink mirror shared by `.plan`, `.apply` and `.destroy` (issues #86, #91):
  - Never run `tofu` in per-target runfiles or the `<name>.work` build output.
    Runfiles are per-target and pruned by Bazel, so `.plan`, `.apply` and
    `.destroy` would resolve relative paths to different trees, and
    `<name>.work` is an action input that must never gain `tofu`'s files or a
    `.terraform.lock.hcl`.
  - On each run, clear symlinks, the old lock and `.terraform/providers`,
    preserve the rest of `.terraform/` and relative local state, and error
    rather than overwriting any regular file blocking a config link, which
    would shadow a configuration file.
  - Never rewrite a `backend` block or pass `-backend-config`, and omit
    `-state` flags when a backend block is present. `tofu` owns path and
    expression resolution, which keeps one behaviour for every backend. There
    is no run-directory lock (`README.md`, "Restrictions").
- Keep deploy launchers (`tf/private/runner.bzl`) standalone like a
  `go_binary`, with each `tf_runner` in its deploy's package:
  - Resolve runfiles and state from `$0` with absolute paths so `bazel run` and
    direct invocation match.
  - Never read `BUILD_WORKSPACE_DIRECTORY` in launchers or the runner; only
    `.fmt` and `pin` may read it.
- Isolate `tofu`'s environment and init flags in the runner (`tofuEnv`):
  - Pin `TF_DATA_DIR` to the run directory's `.terraform` so an inherited
    `TF_DATA_DIR` cannot make `tofu init -get=false` miss the module manifest
    at run time.
  - Strip `TF_PLUGIN_CACHE_DIR` so `tofu` uses only the vendored plugin tree.
  - Pass `-reconfigure` to `tofu init` (issue #92) so saved backend state in
    `.terraform/` cannot block an edited `backend` block until `bazel clean`.
- Route all work-tree and plugin-tree materialisation through
  `tf/private/work_tree.bzl` (`materialize`, `materialize_plugin_tree`,
  `work_tree_root`, `plugin_tree_root`) across `library.bzl`, `deploy.bzl` and
  `test.bzl`. Keep `<name>.plugins/` a sibling of `<name>.work`: validation
  copies `<name>.work` into a scratch directory under `$TMPDIR` and deletes it
  on exit because `tofu init` writes next to the configuration and sandbox
  inputs are read-only, while `-plugin-dir` reads plugins in place without
  copying provider binaries.

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

## Branch / PR conventions

Development branch (`claude/...`); push there, not `main`. Don't open a
PR unless asked. Don't include the model identifier (`claude-opus-4-7`
etc.) in any commit message, PR title, or code artifact.
