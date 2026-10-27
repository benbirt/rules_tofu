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

## Invariants — don't regress these

- No `.terraform.lock.hcl` is accepted in `srcs`/`data`. The only lock
  rules_tofu uses is the root module's `tf_providers.from_lock` file, which
  `@tf_providers//:pin` writes into the user's tree. Every provider download
  is gated on a per-platform
  digest: the `sha256` pinned on its `tf_providers.provider(...)` tag; for
  a tag that pins none, the OpenTofu registry's, recorded as a fact in
  `MODULE.bazel.lock` (keyed `<source>@<version>`); for a locked provider,
  the registry's, accepted only if it is one of the lock's `zh:` hashes
  (`lock_sha256s` in `tf/providers/lock.bzl`). A locked provider's digests
  are resolved and checked by its repository rule when fetched
  (`_locked_downloads`), never in the extension, and are never facts: the
  lock is the whole pin, so a bot that updates only `.terraform.lock.hcl`
  (and `versions.tf`) leaves `MODULE.bazel.lock` valid.
  `//tests/integration:lock_update_test` guards this; don't move the
  lookup back into the extension. The table in
  `toolchain/versions.bzl` pins tofu itself. The repository cache keys on
  these digests. Both extensions declare `reproducible = True`, so their
  repo specs deliberately do *not* appear in `MODULE.bazel.lock`; the
  provider extension's facts do. Facts are validated on read
  (`valid_cached_sha256s`) and treated as a miss if malformed, never
  trusted blindly, and only the facts in use are returned, so stale ones
  drop out.
  `init_action.bzl` fails loudly if a lock file slips into the work tree;
  don't relax that check. The managed lock is never part of the work tree:
  it travels as `TfProviderInfo.lock` (from the copy in the hub repo), and
  the validate action copies it in only *after* that check, as the runner
  does with `--lock-file`. Don't use `-lockfile=readonly`: it also fails on
  lock entries a work tree doesn't use.
- `from_lock` details: one lock per workspace, named `.terraform.lock.hcl`.
  Only the root module's lock counts; a dependency's `from_lock` is
  silently ignored (never read, never an error), as OpenTofu ignores a
  child module's lock and Cargo/npm ignore a dependency's: libraries
  constrain, the root decides. The `@tf_providers` hub is always created
  (so a dependency's `use_repo(tf_providers, "tf_providers")` resolves),
  with an alias for every resolved provider; `:pin` explains itself and
  fails when the root has no lock. A missing lock is an empty one, so `pin` can write
  the first. A lock *entry* the extension can't use (wrong host, `zh:`
  mismatch, version not on the registry) becomes a `_tf_provider_download`
  with `error` set that fails when fetched, never a `fail()` in the
  extension: that would also break `@tf_providers//:pin`, the way to fix
  it. Only an unparseable file fails the extension.
- The lock is parsed by `lockjson` (`tf/private/cmd/lockjson`, using
  `hashicorp/hcl`'s `gohcl`), not in Starlark: don't reintroduce a
  hand-written parser. An extension can't run Bazel-built tools, so
  `tf/providers/lock_reader.bzl` compiles it with rules_go's host Go SDK
  (`@go_host_compatible_sdk_label`), as Gazelle's `go_repository_tools`
  does, into `@rules_tofu_lock_reader` (a non-dev `use_repo_rule` in
  `MODULE.bazel`). Its Go module deps come through `GOPROXY`, checked
  against `go.sum`, under `-mod=readonly`, so `go.mod` must stay tidy for
  `lockjson` too. `lockjson` only parses; what an entry must contain
  (host, `version`, `zh:` hashes, no duplicates) is `lock_entries` in
  `tf/providers/lock.bzl`, unit-tested in Starlark.
- `pin` (`tf/private/cmd/pin`) locks against the `required_providers` of
  every `tf_library`/`tf_deploy` in the main repository, found with
  `bazel query` (run from inside `bazel run`; `$BAZEL` overrides the
  binary). The query runs with `--keep_going` and accepts partial results
  (exit 3): `deps()` follows `providers` into the hub, which has no package
  for a provider until it is locked, so without it the first `pin` fails
  wherever a BUILD file already names the provider (also covered by
  `lock_update_test`). Only `.tf` files in those rules count, a `versions.tf` included:
  don't reintroduce a filesystem walk, which would have to duplicate
  Bazel's own `.bazelignore`/deleted-package/nested-workspace handling. A
  `versions.tf` beside the lock is strongly recommended but optional;
  without one the libraries' constraints are all there is. It extracts only
  the `required_providers` entries into a scratch configuration, one child
  module per directory, so tofu combines them as for nested modules, and
  runs `tofu get` + `tofu providers lock` there from scratch, because
  `providers lock` keeps an existing lock's versions and has no
  `-upgrade`. It leaves out providers the root module tags (the hub passes
  them as `--exclude-source`), and on failure lists the constraints by
  provider and directory, since tofu's error doesn't say where they came
  from. The resulting `constraints` line is the combined
  one; Renovate and Dependabot write their own, and nothing verifies it.
- Provider repos vendor the whole unpacked zip (`package_files`), not just
  the binary, and `materialize_plugin_tree` symlinks all of it: tofu checks
  a lock's `h1:` hash over the entire package directory, so a binary-only
  plugin tree fails `tofu init` against a lock.
- Both module extensions call `module_ctx.extension_metadata` with the
  dev/non-dev split keyed on `root_module_has_non_dev_dependency`, so
  `bazel mod tidy` can manage downstream `use_repo` lines. Root direct
  deps are `tofu_toolchains` only (never the internal `tofu_<platform>`
  repos) and, for providers, only `tf_providers` (the hub), whenever the
  root uses the extension. The per-provider `tf_providers_<ns>_<name>`
  repos are private implementation details: they're never reported as
  direct deps, their files are private, and `:provider` is visible only to
  `@tf_providers//:__subpackages__` (the hub's aliases). BUILD files name
  every provider as `@tf_providers//<ns>/<name>`; don't document or reintroduce
  the per-provider labels (removing them was a breaking change).
  `bazel mod tidy` must leave `MODULE.bazel` unchanged.
- The tofu extension resolves `tofu.version` as "root arbitrates": >1
  root tag fails, one root tag wins unconditionally, agreeing non-root
  requests are honoured, disagreeing non-root requests fail naming the
  versions and modules, no tags means `DEFAULT_VERSION`. The branching
  lives in `toolchain/version_resolution.bzl` as a pure function that
  returns `struct(version, error)` — the extension owns the `fail()` so
  the logic stays unit-testable from `//toolchain/tests`. Don't fold it
  back inline, and don't make it last-write-wins. The providers
  extension stays "root wins, else highest version": a provider is a
  per-configuration artefact (MVS-shaped), the tofu binary is one global
  tool.
- Where a provider zip comes from is decided by the pure functions in
  `tf/providers/source.bzl` (tested from `//tf/providers/tests`):
  `url_template` if set, else the OpenTofu registry's download endpoint,
  for every namespace including `hashicorp/`, so rules_tofu vendors the
  binaries `tofu init` would fetch. Don't reintroduce a special case for
  releases.hashicorp.com: those are different builds with different
  checksums. The registry's `shasum` is checked against a pin before the
  zip is downloaded; the digest still gates the download itself. A tag
  with `url_template` must pin `sha256`. Sources are lower-cased (as
  OpenTofu normalizes addresses) for grouping, the repo name and the
  plugin tree.
- `tf_library` validates inline — the validate stamp goes in
  `DefaultInfo.files` and `bazel build :foo_lib` is the validation
  contract. Don't add a `:foo.validate` sub-target or a `validate_test`
  macro flag. The only test target a `tf_library` generates is
  `:foo.fmt_check`.
- `_ALLOWED_EXTS` in `library.bzl` / `deploy.bzl` includes `.hcl`, which
  technically permits a user to list `.terraform.lock.hcl` in `srcs`.
  The runtime rejection in `init_action.bzl` is the backstop — keep it.
- The build-time validate action and the runtime runner both pick the
  **exec-platform** provider binary. Cross-compilation to a different
  target platform is out of scope and isn't worth designing around
  speculatively.
- The runner expects `--plugin-dir` to be required, and `os.MkdirAll`s
  it on first use so the zero-providers case still presents tofu with
  an extant directory. Don't make the flag optional.
- The runner refuses `apply`/`destroy` when no backend/cloud block is
  detected and `tf_deploy(allow_ephemeral_state = ...)` is unset: local
  state under `bazel-bin` is machine-local and `bazel clean`-able, so
  applying against it orphans live infra. The gate lives in the Go runner
  (backend detection is a runtime filesystem scan), fires before any tofu
  process starts or the state dir is created, and `plan` stays ungated.
  Don't downgrade it to a warning.
- Shared materialization helpers live in `tf/private/work_tree.bzl`
  (`materialize`, `materialize_plugin_tree`, `work_tree_root`,
  `plugin_tree_root`). Both `library.bzl` and `deploy.bzl` should go
  through them; don't reintroduce inline copies. The plugin tree is a
  sibling of the work tree (`<name>.plugins/`, not nested under
  `<name>.work/`), so the validate action's work-tree copy never touches
  provider binaries.
- `module()` in `MODULE.bazel` deliberately has no `version`: the release
  tag is the single source of truth, and publish-to-bcr (or BCR's
  `add_module` on the manual path) patches the real version into the
  registry entry. Don't add a version back, and don't hardcode release
  versions in README/docstring snippets — use the `X.Y.Z` placeholder
  plus a registry link. Keep comments out of the `module(...)` block:
  publish-to-bcr inserts `version` before the first `)` after `module(`,
  so a parenthesis in an in-block comment corrupts the BCR entry (this
  broke 0.1.1's BCR submission).
- `bazel_skylib` is a `dev_dependency`, so the Starlark unit tests that
  load it live in the `//toolchain/tests` and `//tf/providers/tests`
  subpackages: `toolchain/BUILD.bazel` (it holds `:toolchain_type`) and
  `tf/providers/BUILD.bazel` are loaded by downstream modules and must
  never reference a dev-only repo.
- `bazel_compatibility = [">=8.5.0"]` in `module()` is the supported
  floor: 8.5 is the first Bazel with extension facts, so
  `tf/providers/extensions.bzl` uses `module_ctx.facts` unconditionally
  (no `hasattr` fallback). CI runs Linux and macOS on both the pinned
  version and 8.5.0; the uncommitted-changes check only runs on the pinned
  jobs, because other Bazel versions rewrite `MODULE.bazel.lock` in their
  own format. Raising the floor means bumping `bazel_compatibility`, the CI
  matrix and the README together.
- Modules are never fetched. Every rule writes
  `.terraform/modules/modules.json` (tofu's own install record) with the
  `//tf/private/cmd/modules` action, listing every module call from the root
  down, local ones included, and both the validate action and the runner
  copy it into place and run `tofu init -get=false`. Don't drop
  `-get=false`: without it a call whose `version` rejects the manifest's is
  silently re-downloaded, and local calls rely on the manifest too. A
  non-local call resolves to the `tf_library` in `deps` (transitive) whose
  `module_source` matches its `source`, optionally plus `//<subdir>`;
  registry addresses are compared lower-cased with the default host added
  (`NormalizeSource`), others verbatim. Version constraints are tofu's to
  check, against `module_version`; don't reimplement them. The manifest
  format is internal to tofu, so `module_dependency_<version>_test` runs for
  every entry of `KNOWN_VERSIONS`. Files from other repositories sit at
  `external/<canonical repo>/` in the work tree (`work_tree_path`); a
  relative `source` may not cross into or out of one, since canonical names
  are Bazel's to change. Rule paths (`work_tree_root`, `package_dir`, the
  runner's runfiles paths) include `label.workspace_root`, so a library
  in another repository builds on its own.
- The `.bazelversion` copies (root, `e2e/smoke`,
  `tests/integration/broken_workspace`,
  `tests/integration/broken_module`,
  `tests/integration/lock_dependency`,
  `tests/integration/lock_update`,
  `tests/integration/module_dependency`) must stay byte-identical: nested
  modules are separate bazelisk boundaries that never see the root pin.
  CI's lint job `cmp`s every nested copy against the root; bump them all
  together.
- The `--deleted_packages` lines in `.bazelrc` are owned by
  `bazel run @rules_bazel_integration_test//tools:update_deleted_packages`,
  which rewrites *every* non-comment `--deleted_packages` line with an
  auto-discovered list covering all nested workspaces (including
  `e2e/smoke`). Rerun it after adding/removing nested packages; don't
  hand-edit the lists or add separate one-off lines.
- The release tarball (`release_prep.sh`) excludes `examples/`, `tests/`,
  `toolchain/tests/`, `tf/providers/tests/` and `docs/` but must keep `e2e/` and `.bcr/` — BCR presubmit
  reads the test module and templates from the extracted archive. The
  pathspecs are prefix-anchored, so a package named `tests` nested under a
  shipped directory needs its own exclusion; anything loading a
  `dev_dependency` must not ship.
- `//docs` holds a `starlark_doc_extract` per public `.bzl` file;
  `release_prep.sh` ships their `.binaryproto`s as
  `rules_tofu-<tag>.docs.tar.gz`, the BCR entry's `docs_url`, which
  registry.bazel.build renders. A new public `.bzl` entry point needs a
  target there. Its `deps` are the `all_files` filegroups, not
  `bzl_library`, so shipped packages stay free of dev-only repos.

## Branch / PR conventions

Development branch (`claude/...`); push there, not `main`. Don't open a
PR unless asked. Don't include the model identifier (`claude-opus-4-7`
etc.) in any commit message, PR title, or code artifact.
