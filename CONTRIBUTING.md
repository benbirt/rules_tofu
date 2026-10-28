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
| `tf/` | Public rules (`tf_library`, `tf_deploy`) |
| `tf/providers/` | `tf_providers` Bzlmod extension |
| `tf/private/` | Internal rule implementations |
| `tf/private/cmd/runner/` | Go binary: wraps `tofu` at runtime |
| `tf/private/cmd/dupcheck/` | Go binary: duplicate-variable-key check |
| `toolchain/` | OpenTofu binary download + toolchain registration; `update_versions.sh` pins a release |
| `tests/integration/` | Bazel-in-Bazel negative integration tests |
| `examples/` | End-to-end usage examples |
| `e2e/smoke/` | Standalone downstream module; the BCR presubmit test module |
| `.bcr/` | Bazel Central Registry metadata/source/presubmit templates |

## Hard invariants

Before submitting a PR, make sure you have not violated these:

1. **No `.terraform.lock.hcl` files** — provider pinning is handled exclusively via `MODULE.bazel` SHA256 maps. Never add, commit, or accept `.terraform.lock.hcl` in any rule's `srcs` or `data`.
2. **Build = validation** — `bazel build` on any `tf_library` or `tf_deploy` target must run `tofu init -backend=false && tofu validate`. Do not bypass or defer this.
3. **Private API** — nothing under `tf/private/` is a supported public API. External users load only from `@rules_tofu//tf:defs.bzl` and `@rules_tofu//tf/providers:extensions.bzl`.
4. **`bazel run` only** — `tf_deploy`'s `.plan`/`.apply`/`.destroy` targets require `BUILD_WORKSPACE_DIRECTORY` to be set; they must be invoked via `bazel run`, never executed directly.

## Running the tests

```sh
# Unit tests (Go):
bazel test //tf/private/cmd/...

# Integration tests (Bazel-in-Bazel, slower):
bazel test //tests/integration/...

# Format checks (fmt_check tests) run as part of the full suite:
bazel test //...

# To target a single target's format check, use its `:<name>.fmt_check` label:
bazel test //examples/hello:greet_lib.fmt_check
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
toolchain/update_versions.sh 1.12.1            # add it to KNOWN_VERSIONS
toolchain/update_versions.sh --default 1.12.1  # ...and make it the default
```

The script needs `curl` and
[`cosign`](https://docs.sigstore.dev/cosign/system_config/installation/). It
downloads the release's `SHA256SUMS`, verifies its cosign signature against
the OpenTofu release workflow's identity, and copies the five platform digests
into [`toolchain/versions.bzl`](toolchain/versions.bzl). Don't paste digests
in by hand. Build `//examples/hello:hello` afterwards to check the new binary
downloads, and run the Starlark tests under `//toolchain/tests`.

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
- The release tarball excludes `examples/` and `tests/` but **keeps** `e2e/` and
  `.bcr/`, which the BCR presubmit reads from the extracted archive. See
  [`release_prep.sh`](.github/workflows/release_prep.sh).
