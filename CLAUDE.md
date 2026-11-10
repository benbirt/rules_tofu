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

`bazel build //...` still fails in the sandbox because `rules_go` fetches
`https://go.dev/dl/?mode=json` (proxy 403). CI has open network and is
unaffected. Locally, exercise the parts that don't pull in the runner:

- `bazel build //examples/hello:hello //examples/aws:s3_bucket //examples/gcp:storage_bucket`
- `bazel build //examples/hello:greet_lib //examples/aws:s3_bucket_lib //examples/gcp:storage_bucket_lib`
- `bazel test //examples/...:all` (fmt_check tests only)
- Runner unit tests outside Bazel: copy `main.go` + `main_test.go` to a
  scratch dir with a trivial `go.mod` and `go test ./...` (the canonical
  Bazel run is `bazel test //tf/private/cmd/runner:runner_test`,
  which needs network).

## Where things are documented

- `README.md` is the user documentation: the rules, providers and the lock
  ("Providers"), modules ("Modules"), the toolchain and bringing your own
  binary ("The `tofu` binary"), the work tree and runner ("How it works"),
  "Restrictions", "Non-goals" and "Known gaps".
- `CONTRIBUTING.md` is the maintainer documentation: repository layout,
  the hard invariants in short form with the public `.bzl` entry points,
  running the tests, pinning a new OpenTofu release, formatting, and the
  release and BCR process.
- This file carries only what neither of those should: the sandbox
  bootstrap above, the reasons behind the invariants, and the traps earlier
  changes fell into. When a note here starts describing how the code works
  rather than what must not change, move it to README or CONTRIBUTING and
  leave a pointer. Names in backticks are for `grep`, not descriptions.

## Invariants — don't regress these

### Providers and the lock

- The only `.terraform.lock.hcl` rules_tofu reads is the root module's
  `from_lock` file (README, "Pinning with a lock file"; CONTRIBUTING,
  invariant 1). None is accepted in `srcs`/`data`, and `init_action.bzl`
  fails if one reaches a work tree: don't relax that. `ALLOWED_SRC_EXTS` in
  `tf/private/work_tree.bzl` includes `.hcl`, so a user *can* list a lock
  in `srcs`; the runtime rejection is the backstop. The managed lock
  travels as `TfProviderInfo.lock` and is copied in only *after* that
  check, as the runner does with `--lock-file`. Don't use
  `-lockfile=readonly`: it also fails on entries a work tree doesn't use.
- Every provider download is gated on a per-platform digest: the tag's
  `sha256`; else the registry's, recorded as a fact in `MODULE.bazel.lock`
  (keyed `<source>@<version>`); for a locked provider, the registry's,
  accepted only if it is one of the lock's `zh:` hashes (`lock_sha256s`).
  A locked provider's digests are resolved by its repository rule
  (`_locked_downloads`), never in the extension and never as facts: the
  lock is the whole pin, so a bot that updates only the lock and
  `versions.tf` leaves `MODULE.bazel.lock` valid.
  `//tests/integration:lock_update_test` guards this; don't move the
  lookup back into the extension.
- All three extensions (`tofu`, `tf_providers`, `tf_modules`) declare
  `reproducible = True`, so their repo specs are deliberately absent from
  `MODULE.bazel.lock`; only facts are recorded.
  Facts are validated on read (`valid_cached_sha256s`, `valid_fact`),
  treated as a miss if malformed, and only the facts in use are returned,
  so stale ones drop out.
- The download repository rules (`_tofu_download`, `_tf_provider_download`,
  `_tf_module_repo`) end with `return reproducible(repository_ctx)`
  (`tf/private/repo_metadata.bzl`), so Bazel 9's repo contents cache
  reuses their files across output bases: the nested integration tests
  fetch each provider, tofu and module once per machine. That is sound
  only while every download those rules make is checked against a digest
  their attributes or the lock carry (the registry supplies URLs, never
  contents); a rule that came to fetch anything unpinned must stop
  declaring it. The hub repos are cheap and don't declare it.
- A dependency's `from_lock` is silently ignored (README: libraries
  constrain, the root decides). The `@tf_providers` hub is always created,
  so a dependency's `use_repo` resolves, and a missing lock is an empty
  one, so `pin` can write the first. A lock *entry* the extension can't
  use becomes a `_tf_provider_download` with `error` set that fails when
  fetched, never a `fail()` in the extension: that would also break
  `@tf_providers//:pin`, the way to fix it. Only an unparseable file fails
  the extension.
- The lock is parsed by `lockjson` (`tf/private/cmd/lockjson`, `gohcl`),
  not in Starlark; don't reintroduce a hand-written parser. `lockjson` also
  checks and splits each entry's address with OpenTofu's registry-address
  library, as tofu's own lock reader does (full, lockable, canonical form);
  don't re-implement address rules in Starlark. What else an entry must
  contain is `lock_entries` in `tf/providers/lock.bzl`, unit-tested in
  Starlark. An extension can't run Bazel-built tools, so
  `tf/private/host_tools.bzl` compiles `lockjson` and `modscan` with the
  host Go SDK into `@rules_tofu_host_tools` (a non-dev `use_repo_rule`),
  under `-mod=readonly` via `GOPROXY`: `go.mod` and `go.sum` must stay tidy
  for them too.
- `pin` (`tf/private/cmd/pin`; behaviour in README, "Providers") finds
  `required_providers` with one `bazel query` (`--output=streamed_jsonproto`
  with `module_source` as the only rule attribute, `--keep_going`, exit 3
  accepted: `deps()` follows `providers` into a hub that has no package for
  a provider until it is locked; `$BAZEL` overrides the binary). Don't
  reintroduce a filesystem walk, which would have to duplicate Bazel's
  `.bazelignore`, deleted-package and nested-workspace handling. Files from
  other repositories count only in the directories `tfconfig.ModuleDirs`
  gives, the ones `modscan` reads plus the submodules outside `modules/`
  that counted files call with `//<subdir>` (resolved with `tfconfig.Match`
  like the manifest, to a fixpoint), so a fetched module's examples never
  reach the lock (`tf_modules_test`). Every `_tf_library` in `deps` is
  queried, not only those with `module_source` (issue #60). It locks a
  scratch configuration from scratch,
  because `tofu providers lock` keeps an existing lock's versions and has
  no `-upgrade`, and leaves out providers the root tags
  (`--exclude-source`). The `constraints` line it writes is the combined
  one; Renovate and Dependabot write their own, and nothing verifies it.
- Provider repos vendor the whole unpacked zip (`package_files`) and
  `materialize_plugin_tree` symlinks all of it: a lock's `h1:` hash covers
  the entire package directory, so a binary-only tree fails `tofu init`.
- Where a zip comes from is the pure functions in `tf/providers/source.bzl`
  (tested in `//tf/providers/tests`): `url_template` if set, else the
  OpenTofu registry, for every namespace including `hashicorp/`. Don't
  reintroduce a special case for releases.hashicorp.com: those are
  different builds with different checksums. A tag with `url_template`
  must pin `sha256`. Sources are lower-cased, as OpenTofu normalises them.
- The per-provider `tf_providers_<ns>_<name>` repos are private: never
  reported as direct deps, files private, `:provider` visible only to the
  hub. BUILD files name providers as `@tf_providers//<ns>/<name>`; don't
  document or reintroduce the per-provider labels (removing them was a
  breaking change).

### Modules

- Modules are never fetched at build or run time (README, "How it works").
  Every rule writes `.terraform/modules/modules.json` with the
  `//tf/private/cmd/modules` action and runs `tofu init -get=false`. Don't
  drop `-get=false`: without it a call whose `version` rejects the
  manifest's is silently re-downloaded, and local calls rely on the
  manifest too. The format is tofu's, so `module_dependency_<version>_test`
  runs for every entry of `KNOWN_VERSIONS`.
- Every module `source` is parsed by `tfconfig.ParseSource` in Go, as
  OpenTofu's `addrs.ParseModuleSource` does: registry addresses through
  OpenTofu's `registry-address/v2` (pinned to OpenTofu's version), the git
  and archive forms by hand, tested against OpenTofu's cases. The extension
  asks `modscan parse`; don't parse sources anywhere else, Starlark
  included. go-getter is deliberately not a dependency: its root package
  pulls the S3 and GCS SDKs into every user's host-tools build. Version
  constraints are tofu's to check against `module_version`; don't
  reimplement them. Matching calls to declared modules is `tfconfig.Match`,
  shared by `modscan` and the manifest tool, never Starlark.
- Files from other repositories sit at `external/<canonical repo>/`
  (`work_tree_path`); a relative `source` may not cross that boundary,
  since canonical names are Bazel's to change. Rule paths include
  `label.workspace_root`, so a library in another repository builds on its
  own.
- `tf_modules` (`tf/modules/extensions.bzl`; usage in README, "Modules")
  gives each module one `tf_library` (`:_rules_tofu_module`, with
  `//:_rules_tofu_files` as data: names no file of a module shares, since a
  file beside a target of the same name fails the package, issue #61)
  aliased as `@tf_modules//<path>`; submodules are `//<subdir>` against it,
  never targets of their own, because the hub can't list directories
  without fetching every module. The per-module repos are private, like
  the provider repos. Every fetch is pinned: git by commit (a `?ref=` is
  resolved with `git ls-remote` and recorded as a fact), registry modules
  by location plus commit or digest (a fact keyed `<path>@<version>`),
  archives by a required `sha256`. Don't fetch by tag: tags move. A module
  version bump therefore changes `MODULE.bazel.lock`, unlike a provider
  lock update; the README tells bots to run
  `bazel mod deps --lockfile_mode=update`. The repository rule deletes
  upstream Bazel files and every symlink but those to a file of the module
  (`_tidy`; README, "From a registry, git or an archive"), then runs
  `modscan`; an undeclared call fails the fetch naming the tag to add.
  Walk fetched trees with `walk` in `tf/private/fetched_tree.bzl`, never
  by `is_dir` alone: it is true for a symlink to a directory, and a link
  back to an ancestor loops, while Bazel's `glob` fails the package on one
  (issue #61). Generated libraries name providers as
  `@tf_providers//...`, which they can see only because rules_tofu's own
  MODULE.bazel imports the hub through a non-dev `use_extension`; keep
  that. They can't carry the providers of a submodule outside `modules/`
  that callers reach by `//<subdir>`: the fetch never sees the callers
  (issue #76). Instead the manifest action checks every walked module's
  `required_providers` against the rule's `TfProviderInfo` addresses
  (`--provider`) and fails naming the `@tf_providers//<ns>/<name>` to add,
  before `tofu init` fails less helpfully; `missing_provider` and
  `tf_modules_test` step 3 cover it. Don't try to add the providers
  automatically: provider labels must be dependencies before analysis.
  `//tests/integration:tf_modules_test` covers git (with a moved
  tag), archive and registry modules; `examples/modules/{label,stack,git}`
  use real modules, which CI plans: keep them small (light providers only).
- The manifest tool also reads the `run { module { source = ... } }` blocks
  of a tf_test's test files (`--test-dir`; `tfconfig.TestFiles`,
  `ReadTestCalls`, with `.tofutest` over `.tftest`) and records each under
  tofu's key, `test.<file path with dots>.<run>` (`TestRunKey`; verified
  against 1.12 for root-level, `tests/` and nested directories; tofu drops
  only a `.tftest.hcl` extension, so `.tofutest.hcl`, `.tftest.json` and
  `.tofutest.json` stay in the key, issue #84), resolving
  a local source against the root module's directory, not the test file's,
  as tofu does. `pin` queries `_tf_test` too and feeds those calls into the
  `//<subdir>` fixpoint; `tfconfig.ModuleDirs` keeps ignoring a fetched
  module's own `tests/`, which `glob(["**"])` brings along as data.

### Extensions and the toolchain

- All three extensions call `module_ctx.extension_metadata` with the
  dev/non-dev split keyed on `root_module_has_non_dev_dependency`. Root
  direct deps are the hubs only, `tofu_toolchains`, `tf_providers` and
  `tf_modules`, never the internal per-platform, per-provider or
  per-module repos. `bazel mod tidy` must leave
  `MODULE.bazel` unchanged; CI's lint job checks it.
- `tofu.version` is "root arbitrates": more than one root tag fails, one
  root tag wins unconditionally, agreeing non-root requests are honoured,
  disagreeing ones fail naming the versions and modules, no tags means
  `DEFAULT_VERSION`. The branching is a pure function in
  `toolchain/version_resolution.bzl` returning `struct(version, error)`,
  tested from `//toolchain/tests`; the extension owns the `fail()`. Don't
  fold it inline or make it last-write-wins. Providers stay "root wins,
  else highest version": a provider is a per-configuration artefact, the
  tofu binary is one global tool.
- OpenTofu is downloaded, not built from source, and that is a decision,
  not a gap (`versions.bzl` has the reasoning; README, "Non-goals"). Don't
  reintroduce a from-source TODO. `toolchain/versions.bzl` is written by
  `bazel run //tools:update_versions` after cosign verification
  (CONTRIBUTING, "Pinning a new OpenTofu release"); don't paste digests in
  by hand. The escape hatch is a root module registering its own
  `opentofu_toolchain` (README, "The `tofu` binary");
  `//tests/integration:custom_toolchain_test` guards that root
  registrations keep winning. Running the `terraform` binary is a
  documented non-goal, not a feature to add on request without redesigning
  `tf/providers/source.bzl`.
- `bazel_compatibility = [">=8.5.0"]` is the supported floor: 8.5 is the
  first Bazel with extension facts, so `tf/providers/extensions.bzl` uses
  `module_ctx.facts` unconditionally (no `hasattr` fallback). CI runs Linux
  and macOS on the pinned version and 8.5.0; the uncommitted-changes check
  runs only on the pinned jobs, because other versions rewrite
  `MODULE.bazel.lock` in their own format. The Bazel inside the integration
  tests is the version of the Bazel running them, not `.bazelversion`'s:
  `bazel_binaries.download` reads it from `@running_bazel`
  (`tests/integration/running_bazel.bzl`, `native.bazel_version`), so
  `USE_BAZEL_VERSION=8.5.0`, in CI or at a desk, tests the nested builds on
  8.5.0 too. They once ran on the pinned version whatever ran the tests
  (issue #88); `nested_bazel.sh` fails a test whose nested Bazel isn't the
  running one, from `RUNNING_BAZEL_VERSION` in every test's `env`. `MODULE.bazel`
  names that `.bzl` file although releases leave `tests` out: its one use
  is a dev dependency, so a dependent module never loads it. Raising the
  floor means bumping `bazel_compatibility`, the CI matrix and the README
  together.

### Rules and the runner

- `tf_library` validates inline: the validate stamp is in
  `DefaultInfo.files` and `bazel build :foo_lib` is the validation
  contract. Don't add a `:foo.validate` sub-target or a `validate_test`
  flag; the only generated test is `:foo.fmt_check`. Tests of the
  configuration itself (`tofu test`) are the `tf_test` rule
  (`tf/private/test.bzl`, README "Tests"), a target of its own with its own
  `deps` (helper modules) and `providers`, never a sub-target of a library.
- Test files (`TEST_SRC_EXTS`) are rejected in `tf_library`/`tf_deploy`
  `srcs` (`check_no_test_files`; `test_file_in_srcs`): tofu reads them from
  the root module's directory, so a test in a library's srcs would ship to
  and run in every dependant. `ALLOWED_SRC_EXTS` still admits `.hcl`, so
  the analysis-time check is the gate. Fetched modules' own tests arrive as
  `data`, which is not checked, and are ignored unless a run loads them.
- A `tf_test`'s root module is the package of its `module` attribute, not
  its own, so tests can sit in a `tests/` sub-package. It is read from the
  provider (`TfLibraryInfo.package_dir`, `TfDeployInfo.package_dir`), never
  from `ctx.attr.module.label`, which for an `alias` is the alias's own label
  (issue #87; `examples/hello/tests:greet_alias_test` covers it); its `srcs` must be in
  one directory at or below that root, passed as `-test-directory` to init,
  validate and test alike (`test_dirs_split`). `tofu init -get=false` needs
  the same flag or it misses the run blocks' modules (verified on 1.12).
- The runner's `--command=test` (`tf/private/cmd/runner/test.go`) copies
  the work tree into `$TEST_TMPDIR` (runfiles are Bazel's), passes
  `-backend=false` to init (`tofu test` keeps state in memory and ignores
  backends; without the flag init contacts the backend), needs no
  `--state-dir` or `--output-root`, never writes into runfiles
  (an absent plugin tree gets an empty directory under `$TEST_TMPDIR`), and
  fails on `errored_test.tfstate`, which tofu writes with exit 0 when a
  run's destroy fails. Don't route tests through the plan/apply path or its
  ephemeral-state gate.
- `mock_provider` still needs the provider package in `.terraform/providers`
  (verified: tofu fails without it), so a tf_test's plugin tree is built
  for every test. Real infrastructure is gated by Bazel, not by parsing run
  blocks for `command = apply`: the macro adds `block-network` unless
  `allow_network = True`, which adds `external` and `requires-network`.
- The validate action and the runner both pick the **exec-platform**
  provider binary (README, "Known gaps"). Cross-compilation to another
  target platform is out of scope; don't design around it speculatively.
  The `tofu` toolchains are therefore constrained on the exec platform only
  (`exec_compatible_with`, no `target_compatible_with`), so a `--platforms`
  other than the host's still resolves one (issue #105); the Go tools the
  rules run are all `cfg = "exec"`.
- The runner requires `--plugin-dir` and `os.MkdirAll`s it, so the
  zero-providers case still gives tofu an extant directory. Don't make the
  flag optional.
- The runner refuses `apply`/`destroy` unless the deploy's state is durable
  or `allow_ephemeral_state` is set (README, "State and backends"): state
  under `bazel-bin` is `bazel clean`-able, so applying against it orphans
  live infrastructure. Durable means a remote backend or `cloud` block, or
  a `backend "local"` whose `path` and `workspace_dir` are both literal
  absolute paths outside `--output-root` (`bazel-out`; symlinks followed, a
  missing tail allowed: `localBackendEphemeral`). No backend block, or any
  other local one, is ephemeral. The gate is in the Go runner (backend
  detection is a filesystem scan of the launcher's work tree), fires before
  any tofu process starts or the state dir exists, and `plan` stays
  ungated. Don't downgrade it to a warning, and don't add a second rule
  beside it.
- tofu runs in the deploy's run directory, `<state-dir>/work/<package-dir>`
  (`rundir.go`): a symlink mirror of the launcher's runfiles work tree that
  `.plan`, `.apply` and `.destroy` share (issues #86, #91). Not a runfiles
  tree, which is per target and Bazel's to prune; not the declared
  `<name>.work` tree, which the validate action copies wholesale and which
  must never hold a `.terraform.lock.hcl`. Each run removes the mirror's
  links and tofu's lock, keeps everything else (`.terraform/` less its
  `providers`, a local backend's relative state), and re-links; a leftover
  file in the way of a configuration file is an error, never overwritten.
  The runner never rewrites a backend block (no `-backend-config`, and no
  `-state` flags when one is present): tofu owns path and expression
  resolution, which is what keeps one behaviour for every backend. There is
  no lock on the run directory (README, "Restrictions").
- The launchers (`tf/private/runner.bzl`) are ordinary executables, as a
  `go_binary` is: they find their runfiles tree and the state dir from `$0`
  and pass absolute paths, so `bazel run` and a direct run of the built
  file behave alike. Neither they nor the runner read
  `BUILD_WORKSPACE_DIRECTORY`; only `.fmt` and `pin`, which write to the
  checkout, do. A `tf_runner` must be in its deploy's package (checked at
  analysis), since both are found relative to the launcher.
- The runner sets `TF_DATA_DIR` to the run directory's `.terraform`, drops
  `TF_PLUGIN_CACHE_DIR` (`tofuEnv`) and inits with `-reconfigure` (issue
  #92); keep all three.
- Materialisation helpers live in `tf/private/work_tree.bzl`
  (`materialize`, `materialize_plugin_tree`, `work_tree_root`,
  `plugin_tree_root`); `library.bzl` and `deploy.bzl` go through them,
  never inline copies. The plugin tree is a sibling of the work tree
  (`<name>.plugins/`), so the validate action's work-tree copy never
  touches provider binaries.

### Repository hygiene

- `module()` in `MODULE.bazel` has no `version`; the tag is the source of
  truth (CONTRIBUTING, "Releasing"). Don't hardcode release versions in
  README or docstrings: use `X.Y.Z` plus a registry link. Keep comments
  out of the `module(...)` block: publish-to-bcr inserts `version` before
  the first `)` after `module(`, so a parenthesis in a comment corrupts the
  BCR entry (this broke 0.1.1's submission).
- `bazel_skylib` is a `dev_dependency`, so Starlark unit tests live in the
  `//toolchain/tests`, `//tf/providers/tests` and `//tf/modules/tests`
  subpackages. `toolchain/BUILD.bazel` (it holds `:toolchain_type`),
  `tf/providers/BUILD.bazel` and `tf/modules/BUILD.bazel` are loaded by
  downstream modules and must never reference a dev-only repo.
- The release tarball's exclusions are listed in CONTRIBUTING
  ("Releasing") and `release_prep.sh`. The pathspecs are prefix-anchored,
  so a `tests` package nested under a shipped directory needs its own
  exclusion, and anything loading a `dev_dependency` must not ship. The
  `//docs` targets (CONTRIBUTING, invariant 3) take `all_files` filegroups
  as `deps`, not `bzl_library`, for the same reason.
- The `.bazelversion` copies (root, `e2e/smoke` and every
  `tests/integration/*` workspace) must stay byte-identical: nested
  modules are separate bazelisk boundaries that never see the root pin.
  CI's lint job `cmp`s them against the root; bump them all together.
- The `--deleted_packages` lines in `.bazelrc` are owned by
  `bazel run @rules_bazel_integration_test//tools:update_deleted_packages`,
  which rewrites every non-comment line with an auto-discovered list
  covering all nested workspaces. Rerun it after adding or removing nested
  packages; don't hand-edit the lists or add one-off lines.

## Branch / PR conventions

Development branch (`claude/...`); push there, not `main`. Don't open a
PR unless asked. Don't include the model identifier (`claude-opus-4-7`
etc.) in any commit message, PR title, or code artifact.
