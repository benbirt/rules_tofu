# Contributing to rules_tofu

Thank you for your interest in contributing!

## Development setup

You need:

- [Bazel](https://bazel.build/) (version pinned in [`.bazelversion`](.bazelversion))
- A C toolchain (for `rules_go` compilation)

Clone the repo and build everything:

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
| `tf/providers/` | `tf_providers` Bzlmod extension |
| `tf/modules/` | `tf_modules` Bzlmod extension |
| `tf/private/` | Internal rule implementations |
| `tf/private/cmd/runner/` | Go binary: wraps `tofu` at runtime |
| `tf/private/cmd/dupcheck/` | Go binary: duplicate-variable-key check |
| `tf/private/cmd/{pin,lockjson,modscan,modules}/` | Go binaries behind `@tf_providers//:pin`, the lock reader, `tf_modules` and the modules manifest |
| `toolchain/` | OpenTofu binary download + toolchain registration |
| `docs/` | `starlark_doc_extract` per public `.bzl` file; shipped as the BCR entry's `docs_url` |
| `tools/` | Maintainer tools (dev-only, not in the release tarball): `update_versions` pins an OpenTofu release |
| `tests/integration/` | Bazel-in-Bazel integration tests |
| `examples/` | End-to-end usage examples |
| `e2e/smoke/` | Standalone downstream module; the BCR presubmit test module |
| `.bcr/` | Bazel Central Registry metadata/source/presubmit templates |

## Hard invariants

Before submitting a PR, make sure you have not violated these:

1. **One lock, the root module's** — the only `.terraform.lock.hcl` rules_tofu reads is the one `tf_providers.from_lock` names in the root module, written by `bazel run @tf_providers//:pin`. Never accept a `.terraform.lock.hcl` in any rule's `srcs` or `data`, and never relax the check in `init_action.bzl` that fails when one reaches a work tree. Every provider download is gated on a digest: the `sha256` on its `tf_providers.provider(...)` tag, the registry's digest recorded as a fact in `MODULE.bazel.lock`, or one of the lock's `zh:` hashes for a locked provider.
2. **Build = validation** — `bazel build` on any `tf_library`, `tf_deploy` or `tf_test` target must run `tofu init -backend=false && tofu validate` (a `tf_test` with its test directory). Do not bypass or defer this.
3. **Private API** — nothing under `tf/private/` is a supported public API. External users load only from `@rules_tofu//tf:defs.bzl`, `@rules_tofu//tf/providers:extensions.bzl`, `@rules_tofu//tf/modules:extensions.bzl`, `@rules_tofu//toolchain:extensions.bzl` and `@rules_tofu//toolchain:toolchain.bzl`. Each has a `starlark_doc_extract` target in `docs/`; a new public `.bzl` entry point needs one too.
4. **Launchers are ordinary executables** — `tf_deploy`'s `.plan`/`.apply`/`.destroy` launchers find everything from their own location (the runfiles tree beside them, the deploy's state directory in the same output directory), so they work under `bazel run` and when the built file is run directly. Neither they nor the runner may read `BUILD_WORKSPACE_DIRECTORY` or the checkout. tofu runs in the deploy's run directory (`<name>.rules_tofu-state/work/`), never in a runfiles tree or the declared work tree, and the runner never rewrites a backend block: tofu resolves it. (The tools that write to the checkout, `.fmt` and `@tf_providers//:pin`, do require `bazel run`.)

## Running the tests

```sh
# Unit tests (Go):
bazel test //tf/private/cmd/...

# Unit tests (Starlark, via bazel_skylib's unittest):
bazel test //toolchain/tests/... //tf/providers/tests/... //tf/modules/tests/...

# Integration tests (Bazel-in-Bazel, slower):
bazel test //tests/integration/...

# The same on another Bazel, 8.5.0 (the supported floor) here, as CI does.
# The Bazel the integration tests run inside themselves is the version of
# the one running them, so this tests the nested builds on 8.5.0 too.
# Another Bazel rewrites MODULE.bazel.lock in its own format; restore it:
USE_BAZEL_VERSION=8.5.0 bazel test //tests/integration/...
git checkout -- MODULE.bazel.lock

# Format checks (fmt_check tests) run as part of the full suite:
bazel test //...

# To target a single target's format check, use its `:<name>.fmt_check` label:
bazel test //examples/hello:greet_lib.fmt_check

# `tofu test` tests (tf_test), run offline with the network blocked:
bazel test //examples/hello/tests:all
```

## Starlark formatting

All `.bzl` and `BUILD.bazel` files must be formatted with
[buildifier](https://github.com/bazelbuild/buildtools/tree/main/buildifier),
which is pinned as a dev dependency in `MODULE.bazel`:

```sh
bazel run @buildifier_prebuilt//:buildifier -- -r .
```

CI enforces this; PRs with formatting violations will fail.

## Opening a PR

1. Fork the repo and create a branch.
2. Make your changes and ensure `bazel build //...` and `bazel test //...` both pass.
3. Run `bazel run @buildifier_prebuilt//:buildifier -- -r .` and commit any formatting changes.
4. Open a PR against `main` with a clear description of what changed and why.

## Pinning a new OpenTofu release

```sh
bazel run //tools:update_versions -- 1.12.1            # add it to KNOWN_VERSIONS
bazel run //tools:update_versions -- --default 1.12.1  # ...and make it the default
```

The tool downloads the release's `SHA256SUMS` with the host's `curl`, verifies
its cosign signature against the OpenTofu release workflow's identity with the
[cosign](https://github.com/sigstore/cosign) release pinned in `MODULE.bazel`,
and copies the five platform digests into
[`toolchain/versions.bzl`](toolchain/versions.bzl). Don't paste digests in by
hand.

Nothing else uses a version that is only in `KNOWN_VERSIONS`, so check the new
binary with the integration tests generated for each entry, named with the
version's dots as underscores:

```sh
bazel test //tests/integration:module_dependency_1_12_1_test //tests/integration:fmt_1_12_1_test
```

With `--default`, the examples, `e2e/smoke` and the other integration tests
use the new version too.

## Go version floor

`tf_providers.from_lock` and `tf_modules.module` compile `lockjson` and
`modscan` with the host Go SDK of the *downstream* workspace
(`tf/private/host_tools.bzl`), so the `go` directive in `go.mod` is a floor
for every user that registers a Go SDK, not only for this repository. It
can't be lower than the pinned dependencies' own directives (`hcl/v2` sets
it today), and Dependabot's `gomod` updates raise it with them. Call a
raised floor out in the release notes.

## Releasing

Releases are driven by SemVer tags and published to the
[Bazel Central Registry](https://registry.bazel.build/) so downstreams can use
`bazel_dep(name = "rules_tofu", version = "x.y.z")`.

The tag is the **single source of truth** for the version: `version` stays
unset in `MODULE.bazel` (see the NOTE there), and the BCR entry gets the
release version patched into its `MODULE.bazel` from the tag. There is nothing
to bump before tagging.

1. Tag the commit to release as `vX.Y.Z` and push the tag.
2. [`.github/workflows/release.yaml`](.github/workflows/release.yaml) fires: it
   runs [`release_prep.sh`](.github/workflows/release_prep.sh) to build
   `rules_tofu-vX.Y.Z.tar.gz`, attaches build attestations, and publishes the
   GitHub Release. The generated release notes include the correct
   `bazel_dep(...)` snippet for that version.
3. Get the version into the BCR:
   - **First submission / manual:** clone `bazelbuild/bazel-central-registry`,
     run `bazel run //tools:add_module`, point it at the release tarball and the
     [`.bcr/`](.bcr) templates, and open the PR. A brand-new module's first PR
     gets extra maintainer review.
   - **Automated (opt-in):** fork BCR to `benbirt/bazel-central-registry`, add a
     classic `BCR_PUBLISH_TOKEN` PAT (`repo` + `workflow` scopes), and set the
     repository variable `PUBLISH_TO_BCR=true`. The `publish` job then opens the
     BCR PR automatically on every tag.

Notes:

- Don't add a `version` to `MODULE.bazel`: the automated path relies on
  publish-to-bcr's version patching, and the manual `add_module` path likewise
  generates a `module_dot_bazel_version.patch` when the archive's version
  differs from the one being submitted.
- Published BCR versions are immutable (add-only) — never edit a shipped version;
  to retract one, yank it via `.bcr/metadata.template.json`.
- The release tarball excludes `examples/`, `tests/`, `tools/`, `docs/` and
  the `toolchain/tests/`, `tf/providers/tests/` and `tf/modules/tests/`
  packages (everything that loads a `dev_dependency`), but **keeps** `e2e/`
  and `.bcr/`, which the BCR presubmit reads from the extracted archive.
  `docs/` feeds the separate docs archive instead. See
  [`release_prep.sh`](.github/workflows/release_prep.sh).
