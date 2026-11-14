# Contributing to rules_tofu

## Development setup

Prerequisites:

- [Bazel](https://bazel.build/) at the version in
  [`.bazelversion`](.bazelversion)
  ([Bazelisk](https://github.com/bazelbuild/bazelisk) selects it automatically)
- A C toolchain (`rules_go` compiles Go binaries with cgo enabled)
- `git` on `PATH` (`tf_modules` runs the host `git` to fetch git sources)

Clone, build and test:

```sh
git clone https://github.com/benbirt/rules_tofu.git
cd rules_tofu
bazel build //...
bazel test //...
```

## Repository layout

| Path | Purpose |
|------|---------|
| `tf/` | Public rules (`tf_library`, `tf_deploy`, `tf_test`) |
| `tf/providers/` | `tf_providers` module extension |
| `tf/modules/` | `tf_modules` module extension |
| `tf/private/` | Rule implementations and work-tree materialisation |
| `tf/private/cmd/{runner,dupcheck,pin,lockjson,modscan,modules}/` | Go binaries: runtime `tofu` wrapper, duplicate-variable check, `@tf_providers//:pin`, lock reader, module scanner and module manifest writer |
| `tf/private/cmd/internal/{flags,tfconfig}/` | Shared Go packages: repeatable CLI flags and HCL / module-source parsing |
| `toolchain/` | OpenTofu binary download and toolchain registration |
| `docs/` | `starlark_doc_extract` target per public `.bzl` file, shipped as the BCR `docs_url` archive |
| `tools/` | Maintainer tools (excluded from releases): `update_versions` pins an OpenTofu release |
| `tests/integration/` | Bazel-in-Bazel integration tests ([`tests/integration/README.md`](tests/integration/README.md)) |
| `examples/` | Usage examples |
| `e2e/smoke/` | Standalone consumer module; BCR presubmit test module |
| `.bcr/` | Bazel Central Registry metadata, source and presubmit templates |

## Hard invariants

1. One lock file, owned by the root module. `rules_tofu` reads only the root
   module's `tf_providers.from_lock` `.terraform.lock.hcl`, written by
   `bazel run @tf_providers//:pin`. Do not accept `.terraform.lock.hcl` in
   `srcs` or `data`, and do not relax the check in `tf/private/init_action.bzl`
   that fails when one reaches a work tree. Every provider download must be
   gated on a SHA-256 digest: the `sha256` on its `tf_providers.provider` tag,
   the registry digest recorded as a fact in `MODULE.bazel.lock`, or one of the
   lock file's `zh:` hashes.
2. Building validates. `bazel build` on any `tf_library`, `tf_deploy` or
   `tf_test` target runs `tofu init -backend=false` and `tofu validate` (with
   the test directory for `tf_test`). Do not bypass or defer this.
3. Nothing under `tf/private/` is public API. Downstream modules load only
   `@rules_tofu//tf:defs.bzl`, `@rules_tofu//tf/providers:extensions.bzl`,
   `@rules_tofu//tf/modules:extensions.bzl`,
   `@rules_tofu//toolchain:extensions.bzl` and
   `@rules_tofu//toolchain:toolchain.bzl`. Every public `.bzl` entry point has
   a `starlark_doc_extract` target in `docs/BUILD.bazel`; add one for any new
   entry point.
4. Deploy launchers are ordinary executables. `tf_deploy`'s `.plan`, `.apply`
   and `.destroy` scripts find their runfiles tree and state directory relative
   to `$0`, so they work both under `bazel run` and when run directly. Neither
   the launchers nor the runner may read `BUILD_WORKSPACE_DIRECTORY` or the
   checkout; only `:<name>.fmt` and `@tf_providers//:pin`, which write to the
   checkout, require `bazel run`. `tofu` runs in
   `<name>.rules_tofu-state/work/<pkg>`, never in runfiles or `<name>.work/`,
   and resolves `backend` blocks itself without runner rewrites.

## Running the tests

```sh
# Go unit tests:
bazel test //tf/private/cmd/...

# Starlark unit tests (bazel_skylib unittest):
bazel test //toolchain/tests/... //tf/providers/tests/... //tf/modules/tests/...

# Bazel-in-Bazel integration tests:
bazel test //tests/integration/...

# Integration tests on Bazel 8.5.0 (the supported floor). @running_bazel puts
# the nested builds on 8.5.0 too; restore MODULE.bazel.lock afterwards because
# 8.5.0 rewrites it in its own format:
USE_BAZEL_VERSION=8.5.0 bazel test //tests/integration/...
git checkout -- MODULE.bazel.lock

# A single target's format check (`bazel test //...` runs them all):
bazel test //examples/hello:greet_lib.fmt_check

# `tofu test` targets (offline, network blocked):
bazel test //examples/hello/tests:all

# Real-module plans and the standalone BCR smoke module (excluded from //...
# via --deleted_packages in .bazelrc), as CI runs them:
bazel run //examples/modules/label:label.plan
bazel run //examples/modules/stack:stack.plan
bazel run //examples/modules/git:git.plan
(cd e2e/smoke && bazel build //... && bazel test //...)
```

On the pinned Bazel version, CI runs builds, tests and example plans with
`--lockfile_mode=error` and checks that `git status --porcelain` stays empty.

## Formatting and lint

CI's `lint` job runs `gofmt -l .`, `buildifier -mode=check -r .` and
`buildifier -lint=warn -r .` as separate steps, plus `bazel mod tidy` to check
that `MODULE.bazel` is unchanged. Locally, `-mode` defaults to `fix`, so one
`buildifier` invocation both formats Starlark files in place and prints lint
warnings:

```sh
gofmt -w .
bazel run @buildifier_prebuilt//:buildifier -- -lint=warn -r .
bazel mod tidy
```

When adding a nested Bazel module under `e2e/` or `tests/integration/`:

1. Copy the root `.bazelversion` into it. CI checks that every nested module has
   a `.bazelversion` byte-identical to the root file.
2. Add its four `bazel-*` symlinks (`bin`, `out`, `testlogs`, `<dir-name>`) to
   `.bazelignore`. CI checks all four entries exist.
3. Run `bazel run @rules_bazel_integration_test//tools:update_deleted_packages`
   to update `--deleted_packages` in `.bazelrc`.

Open PRs against `main` from a branch on a fork once the build, tests and lint
checks pass.

## Pinning a new OpenTofu release

```sh
bazel run //tools:update_versions -- 1.12.1            # add to KNOWN_VERSIONS
bazel run //tools:update_versions -- --default 1.12.1  # also set DEFAULT_VERSION
```

`update_versions` requires `curl` on `PATH`. It downloads the release's
`SHA256SUMS`, verifies its Sigstore signature against the OpenTofu release
workflow identity using the `cosign` binary pinned in `MODULE.bazel`, and
writes the five platform digests into
[`toolchain/versions.bzl`](toolchain/versions.bzl). Do not paste digests in by
hand.

Test an entry that is not the default with its generated per-version
integration tests (dots replaced by underscores):

```sh
bazel test //tests/integration:module_dependency_1_12_1_test //tests/integration:fmt_1_12_1_test
```

With `--default`, the examples, `e2e/smoke` and the remaining integration tests
use the new version too.

## Go version floor

`tf_providers.from_lock` and `tf_modules.module` compile `lockjson` and
`modscan` with the downstream workspace's host Go SDK
(`tf/private/host_tools.bzl`). The `go` directive in `go.mod` is therefore a
floor for every consumer that registers a Go SDK, not only for this repository.
It cannot be lower than the pinned Go dependencies' own `go` directives
(`hcl/v2` sets it today), and Dependabot's `gomod` updates raise it with them.
Mention any increase in the release notes.

## Releasing

Releases are cut from SemVer tags (`vX.Y.Z`) and published to the
[Bazel Central Registry](https://registry.bazel.build/). Leave `version` unset
in `MODULE.bazel`, and keep comments out of `module(...)`: the tag is the sole
version source, and BCR patches the release version into the published
`MODULE.bazel`.

1. Tag the commit as `vX.Y.Z` and push the tag, or trigger
   [`.github/workflows/release.yaml`](.github/workflows/release.yaml) manually
   via `workflow_dispatch` for an existing tag.
2. `release.yaml` runs
   [`.github/workflows/release_prep.sh`](.github/workflows/release_prep.sh) to
   build `rules_tofu-vX.Y.Z.tar.gz` and `rules_tofu-vX.Y.Z.docs.tar.gz`. The
   docs archive comes from `bazel build //docs:all` and backs `docs_url` in
   `.bcr/source.template.json`. The workflow attaches build attestations and
   publishes the GitHub Release with a `bazel_dep` snippet.
3. Publish to the BCR:
   - Manual (required for a module's first submission): in a
     `bazelbuild/bazel-central-registry` clone, run
     `bazel run //tools:add_module` with the release tarball and
     [`.bcr/`](.bcr) templates, then open a PR.
   - Automated (opt-in): fork the BCR to `benbirt/bazel-central-registry`, add
     a classic `BCR_PUBLISH_TOKEN` secret (`repo` and `workflow` scopes), and
     set the repository variable `PUBLISH_TO_BCR=true`. The `publish` job then
     opens the BCR PR on each release.

Published BCR versions are immutable; to retract one, yank it via
`.bcr/metadata.template.json`.

`release_prep.sh` excludes everything that loads a `dev_dependency` from the
source archive: `examples/`, `tests/`, `tools/`, `docs/`, `toolchain/tests/`,
`tf/providers/tests/` and `tf/modules/tests/`. It keeps `e2e/` and `.bcr/` for
BCR presubmit.
