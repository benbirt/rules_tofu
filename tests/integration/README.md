# Integration tests

Bazel-in-Bazel tests that run a nested Bazel on sub-workspaces consuming
`rules_tofu` via `local_path_override`. Negative cases assert that misuses fail
with the promised error message; positive tests exercise end-to-end workflows.

## Layout

```
tests/integration/
├── BUILD.bazel              test targets: broken_workspace_test, one <case>_test
│                            per negative case in other workspaces, positive tests
├── cases.bzl                name|command|target|fragment line encoding
├── running_bazel.bzl        exposes the outer Bazel's version to nested builds
├── nested_bazel.sh          sourced by every driver: nested caches and `bazel`
├── assert_bazel_fails.sh    negative-test driver: runs `bazel build` or `run`
│                            and checks for a non-zero exit and stderr substring
├── *_test.sh                positive-test drivers
├── broken_workspace/        negative cases sharing one MODULE.bazel
│   ├── cases/<case>/        one package per case
│   └── lock/                the workspace's `from_lock` lock and versions.tf
├── broken_module/           negative: duplicate root `tofu.version` tags
├── lock_dependency/         negative: ignored dependency `from_lock` and
│                            unorderable non-root provider versions
├── custom_toolchain/        root `opentofu_toolchain` precedence check
├── lock_update/             positive: `pin`, bot lock bump, root tag conflict
├── module_dependency/       positive: `tf_library` calls across Bazel modules
├── tf_modules/              positive: `tf_modules` git, archive and registry
├── run_directory/           positive: `tf_deploy` launchers and local state
└── fmt/                     positive: `.fmt` and `.fmt_check`
```

Every sub-workspace has its own `MODULE.bazel` and a `.bazelversion`
byte-identical to the root file. All sub-workspace packages are listed under
`--deleted_packages` in the root `.bazelrc` so that the outer Bazel never
analyses the intentionally failing targets when expanding `//...`.

### Sub-workspaces

`broken_workspace/` holds the negative cases that share one `MODULE.bazel` and
runs them as a single test target (`broken_workspace_test`) via
`assert_bazel_fails.sh`. Starting a nested Bazel server and fetching and
analysing `rules_tofu`'s dependencies cost far more than any single case, so
running them against one server makes every case after the first an incremental
build. `assert_bazel_fails.sh` runs every case even after a failure, dumps each
failing case's log, and lists all failures at the end. Cases whose failure is
in the module graph cannot share `broken_workspace/MODULE.bazel`; each gets its
own sub-workspace (`_WORKSPACE_FILES` in `BUILD.bazel`), and each row outside
`broken_workspace` becomes its own `<case>_test` target.

`broken_module/` (`duplicate_root_version_test`, driven by
`assert_bazel_fails.sh`) declares two `tofu.version` tags in its root
`MODULE.bazel` and checks that `bazel build` fails naming the duplicate tags.

`lock_dependency/` is a two-module graph driven by `assert_bazel_fails.sh` as
two separate `_CASES` targets. The root module has no lock, while its
`lib_module` dependency calls `from_lock` on an unreadable file and tags
`acme/unorderable` at `1.9.0` and `2.0.0-rc1`:

- `dependency_lock_ignored_test` builds `@lib_module//:lib` and checks that the
  dependency's `from_lock` is ignored, failing only because `@tf_providers` has
  no `hashicorp/null` package.
- `unorderable_versions_test` builds `@lib_module//:unorderable` and checks
  that the unorderable non-root tags fail when that provider is fetched rather
  than during extension evaluation, leaving `@lib_module//:lib` to fail for its
  own reason.

`custom_toolchain/` (`custom_toolchain_test`, driven by `assert_bazel_fails.sh`
via `_CASES`) registers a root `opentofu_toolchain` whose stand-in `tofu` exits
non-zero with `"custom tofu toolchain selected"`. It verifies that a root
toolchain registration wins over `rules_tofu`'s downloads.

`lock_update/` (`lock_update_test`, driven by `lock_update_test.sh`) covers a
workspace's first `@tf_providers//:pin`, a lock-only provider bump leaving
`MODULE.bazel.lock` untouched under `--lockfile_mode=error`, and resolving a
root `tf_providers.provider` tag conflict via `pin`. See the header of
`lock_update_test.sh` for the steps.

`module_dependency/` (`module_dependency_<version>_test`, driven by
`module_dependency_test.sh`) has `//app` call a `tf_library` in
`network_module` by `module_source`, which calls one in `dns_module`, and
exercises standalone library builds, offline planning and `tf_test`. Because
the module manifest (`.terraform/modules/modules.json`) is an internal OpenTofu
format, `BUILD.bazel` generates one test per version in `KNOWN_VERSIONS`
(`toolchain/versions.bzl`). See the header of `module_dependency_test.sh` for
the steps.

`tf_modules/` (`tf_modules_test`, driven by `tf_modules_test.sh`) turns
`fixtures/network` into a tagged git repository and `fixtures/dns` into an
archive at test time alongside `cloudposse/label/null` from the OpenTofu
registry. It covers offline planning, tree tidying, commit and registry facts,
`from_lock()` with `pin`, and fetch-time error diagnostics; see the header of
`tf_modules_test.sh` for the steps.

`run_directory/` (`run_directory_test`, driven by `run_directory_test.sh`)
exercises `tf_deploy` launchers under `bazel run` and direct execution from
another directory, sharing local state in `//app:deploy`'s run directory under
`bazel-bin`. See the header of `run_directory_test.sh` for the steps.

`fmt/` (`fmt_<version>_test`, driven by `fmt_test.sh`) checks that `.fmt` and
`.fmt_check` act only on `srcs` files by name, including files in
subdirectories, and are omitted when `srcs` is empty. Which file extensions
`tofu fmt` accepts by name is decided by the `tofu` binary, so `BUILD.bazel`
generates one test per version in `KNOWN_VERSIONS`. See the header of
`fmt_test.sh` for the steps.

## Case lists

`BUILD.bazel` defines two negative-case lists, differing only in the nested
Bazel command `assert_bazel_fails.sh` runs:

- `_CASES` (`bazel build`): failures from package loading, analysis, or build
  actions (`TofuModules`, `TofuValidate`, `dupcheck`, repository fetches).
- `_RUN_CASES` (`bazel run`): targets that build cleanly and are refused at run
  time by a `tf_deploy` launcher. These cover the wiring from BUILD attributes
  through the generated launcher script to runner CLI flags, which the runner's
  Go unit tests do not exercise.

Each row encodes to a `name|command|target|fragment` line in the driver's
`CASES` environment variable.

## Running

```sh
bazel test //tests/integration:all
```

The nested Bazel matches the outer Bazel's version via `@running_bazel`
(`running_bazel.bzl`), ignoring each sub-workspace's `.bazelversion`. Those
`.bazelversion` files are kept for running `bazel` inside a sub-workspace by
hand. Every `bazel_integration_test` sets `RUNNING_BAZEL_VERSION` in `env`, and
`nested_bazel.sh` aborts if the nested binary reports a different version. To
run on Bazel 8.5.0 and restore the root lockfile afterwards:

```sh
USE_BAZEL_VERSION=8.5.0 bazel test //tests/integration:all
git checkout -- MODULE.bazel.lock
```

To run specific cases inside `broken_workspace_test`:

```sh
bazel test //tests/integration:broken_workspace_test \
    --test_env=ONLY_CASES=stale_lock,missing_provider --test_output=all
```

## Adding a case

For a loading, analysis, action or launcher failure inside `broken_workspace`:

1. Add `broken_workspace/cases/<case>/BUILD.bazel` and any fixture files the
   failure requires.
2. Add `("<case>", "broken_workspace", "//cases/<case>:<target>", "<stderr-fragment>")`
   to `_CASES` (fails on `bazel build`) or `_RUN_CASES` (builds, fails on
   `bazel run`) in `tests/integration/BUILD.bazel`.
3. Run `bazel run @rules_bazel_integration_test//tools:update_deleted_packages`
   to refresh `--deleted_packages` in `.bazelrc`.

For a module-graph failure (`MODULE.bazel` tag resolution, `bazel_dep`,
`use_repo`), create a standalone sub-workspace:

1. Create `tests/integration/<workspace>/` alongside `broken_module/` with its
   own `MODULE.bazel`, a copy of the root `.bazelversion`, and a target that
   forces evaluation of the broken extension or repository. Module extensions
   are lazy, so verify that building the target triggers it.
2. Add `<workspace>` to `_WORKSPACE_FILES` in `tests/integration/BUILD.bazel`,
   and add `("<case>", "<workspace>", "<target>", "<stderr-fragment>")` to
   `_CASES` or `_RUN_CASES`.
3. List the workspace's four `bazel-*` symlinks (`bin`, `out`, `testlogs`,
   `<workspace>`) in `.bazelignore`. CI's `lint` job checks both these
   `.bazelignore` entries and the `.bazelversion` copy.
4. Run `bazel run @rules_bazel_integration_test//tools:update_deleted_packages`
   to refresh `--deleted_packages` in `.bazelrc`.

Choose a short, stable stderr substring rather than a full sentence so minor
OpenTofu or rule wording changes do not break the assertion.

## Follow-ups

`_RUN_CASES` covers both forms of the runtime ephemeral-state refusal on
`deploy.apply` (no backend, and a `backend "local"` keeping state under
`bazel-bin`), while `run_directory_test` covers the `allow_ephemeral_state`
opt-in and direct launcher execution.

Still untested in Bazel-in-Bazel: a remote backend reaching `tofu`, and hard
`fail()` paths during `tf_providers` tag resolution (duplicate root tags, or
modules disagreeing on `sha256` or `url_template` for a winning version).
