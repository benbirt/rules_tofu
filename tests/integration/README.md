# Integration tests

Bazel-in-Bazel tests that run a nested Bazel on sub-workspaces consuming
`rules_tofu` via `local_path_override`. Negative cases assert that misuses fail
with the promised error message; positive tests exercise end-to-end workflows.

## Layout

```
tests/integration/
├── BUILD.bazel              test targets: one <workspace>_test per negative
│                            sub-workspace, positive tests
├── cases.bzl                name|command|target|fragment line encoding
├── running_bazel.bzl        exposes the outer Bazel's version to nested builds
├── versions.bzl             `tested_versions` selection for per-version tests
├── versions_test.bzl        unit tests for `tested_versions`
├── nested_bazel.sh          sourced by every driver: nested caches and `bazel`
├── assert_bazel_fails.sh    negative-test driver: runs `bazel build` or `run`
│                            and checks for a non-zero exit and stderr substring
├── *_test.sh                positive-test drivers
├── broken_workspace/        negative cases sharing one MODULE.bazel
│   ├── cases/<case>/        one package per case
│   └── lock/                the workspace's `from_lock` lock and versions.tf
├── lock_dependency/         negative: ignored dependency `from_lock` and
│                            `provider` tags
├── custom_toolchain/        root `opentofu_toolchain` precedence check
├── lock_update/             positive: `pin`, bot lock bump, lock/tag conflict
├── module_dependency/       positive: `tf_library` calls across Bazel modules
├── tf_modules/              positive: `tf_modules` git, archive and registry
└── run_directory/           positive: `tf_deploy` launchers and local state
```

Every sub-workspace has its own `MODULE.bazel` and a `.bazelversion`
byte-identical to the root file. All sub-workspace packages are listed under
`--deleted_packages` in the root `.bazelrc` so that the outer Bazel never
analyses the intentionally failing targets when expanding `//...`.

### Sub-workspaces

`BUILD.bazel` generates one `<workspace>_test` target per entry in
`_WORKSPACE_FILES`, driven by `assert_bazel_fails.sh` over all `_CASES` and
`_RUN_CASES` rows for that sub-workspace. Starting a nested Bazel server and
fetching and analysing `rules_tofu`'s dependencies cost far more than any
single case, so running a sub-workspace's cases against one server makes every
case after the first an incremental build. `assert_bazel_fails.sh` runs every
case even after a failure, dumps each failing case's log, and lists all
failures at the end.

`broken_workspace/` (`broken_workspace_test`) holds the negative cases that
share one `MODULE.bazel`. Cases that need a different module graph or toolchain
registration get their own sub-workspace in `_WORKSPACE_FILES`.

`lock_dependency/` (`lock_dependency_test`) is a two-module graph with two
`_CASES` rows. The root module has no lock, while its `lib_module` dependency
calls `from_lock` on an unreadable file and tags `acme/tagged` without
`url_template`:

- `dependency_lock_ignored` builds `@lib_module//:lib` and checks that the
  dependency's `from_lock` is ignored, failing only because `@tf_providers` has
  no `hashicorp/null` package.
- `dependency_tag_ignored` builds `@lib_module//:tagged` and checks that
  `@tf_providers//acme/tagged` fails to load because the root module does not
  pin it. Extension evaluation itself succeeds, so `@lib_module//:lib` still
  fails for its own reason.

`custom_toolchain/` (`custom_toolchain_test`) registers a root
`opentofu_toolchain` whose stand-in `tofu` exits non-zero with
`"custom tofu toolchain selected"`. Its two cases (`custom_toolchain` in
`_CASES` and `custom_toolchain_run` in `_RUN_CASES`) check that the root's
toolchain wins over `rules_tofu`'s downloads both when a library is validated
at build time and when `bazel run @rules_tofu//tofu` is run.

`lock_update/` (`lock_update_test`, driven by `lock_update_test.sh`) keeps its
lock in `infra/` and covers a workspace's first `@tf_providers//:pin`, fetching
only the execution platform's provider package under a non-host `--platforms`, a
lock-only provider bump leaving `MODULE.bazel.lock` untouched under
`--lockfile_mode=error`, and a lock/tag conflict failing at package load while
leaving `pin` runnable to clear it. It also checks that `bazel mod tidy`
succeeds and changes nothing in a copy holding only the files Dependabot's
`bazel` updater fetches. See the header of `lock_update_test.sh` for the steps.

`module_dependency/` (`module_dependency_<version>_test`, driven by
`module_dependency_test.sh`) has `//app` call a `tf_library` in
`network_module` by `module_source`, which calls one in `dns_module`. It
exercises standalone library builds, offline planning, `tf_test` and
`bazel run @rules_tofu//tofu` from `app/`. It also holds five packages for
module and test edge cases:

- `empty`: libraries and a `tf_test` whose root module is empty (issue #119).
- `nested`: a library whose `srcs` reach into subdirectories, its `tf_test`,
  and a `tf_deploy` with no `srcs` of its own.
- `provisioner`: a library whose `local-exec` provisioner runs a script from
  `data` and a `tf_test` run via `bazel test` and directly by relative path
  (issue #148).
- `reader`: exercises `tofu show -json -module` (issue #153) with module calls
  using a variable default, a local value, `_override`, `.tofu` shadowing and
  `.tf.json`, plus a `manual` `:providers` target covering canonical addresses,
  providers implied by a resource and a data source, aliased `provider` blocks
  and a skipped built-in provider. Step 9 also packs `reader/` as an archive
  and fetches it via `tf_modules.module` to verify fetched-module scanning with
  the selected `TOFU_VERSION` release.
- `sub_only`: a library whose `srcs` are all in a subdirectory.

It also applies `//app:app` with no backend block and
`allow_ephemeral_state = True`,
and checks `//app:durable`, whose `backend "local"` uses `_override` and
variable paths: `apply` is refused while `state_root` is relative and succeeds
once `state_root` points outside `bazel-out`. Because the module manifest
(`.terraform/modules/modules.json`) and backend record
(`.terraform/terraform.tfstate`) are internal OpenTofu formats and
`tofu show -json -module` carries no `format_version`, `BUILD.bazel` generates
one test per version returned by `tested_versions(KNOWN_VERSIONS)` in
`versions.bzl` (the oldest and the newest pinned release). See the header of
`module_dependency_test.sh` for the steps.

`tf_modules/` (`tf_modules_test`, driven by `tf_modules_test.sh`) turns
`fixtures/network` into a tagged git repository and `fixtures/dns` into an
archive at test time alongside `cloudposse/label/null` from the OpenTofu
registry. It covers `pin` across fetched modules, implied providers, generated
configuration built on demand and `select()` branches read separately, offline
planning, OpenTofu-evaluated module sources and implied providers in generated
libraries (dropping built-in providers), tree tidying, commit and registry
facts, fetch-time error diagnostics, regenerating a library on a new pin, and
failing validation on an invalid fetched module. See the header of
`tf_modules_test.sh` for the steps.

`run_directory/` (`run_directory_test`, driven by `run_directory_test.sh`)
exercises `tf_deploy` launchers under `bazel run` and direct execution from
another directory, sharing local state in `//app:deploy`'s run directory under
`bazel-bin`. Its `//vars` package checks `-var` and `-var-file` passing across
validation, `tofu init`, launchers, CLI overrides and `tf_test`. See the header
of `run_directory_test.sh` for the steps.

## Case lists

`BUILD.bazel` defines two negative-case lists, differing only in the nested
Bazel command `assert_bazel_fails.sh` runs:

- `_CASES` (`bazel build`): failures from package loading, analysis, or build
  actions (`TofuModules`, `TofuValidate`, `dupcheck`, repository fetches).
- `_RUN_CASES` (`bazel run`): `custom_toolchain_run` runs `@rules_tofu//tofu`
  with the stand-in binary. The `tf_deploy` launcher cases cover both a target
  that builds cleanly and is refused at run time by the runner (exercising the
  wiring from BUILD attributes through the generated launcher script to runner
  CLI flags, which the runner's Go unit tests do not cover) and one that fails
  in the build before reaching the runner.

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

To run specific cases inside `broken_workspace_test`, `custom_toolchain_test`
or `lock_dependency_test`:

```sh
bazel test //tests/integration:broken_workspace_test \
    --test_env=ONLY_CASES=stale_lock,strict_deps_provider --test_output=all
```

## Adding a case

For a loading, analysis, action or launcher failure inside `broken_workspace`:

1. Add `broken_workspace/cases/<case>/BUILD.bazel` and any fixture files the
   failure requires.
2. Add `("<case>", "broken_workspace", "//cases/<case>:<target>", "<stderr-fragment>")`
   to `_CASES` (fails on `bazel build`) or `_RUN_CASES` (fails on
   `bazel run`) in `tests/integration/BUILD.bazel`.
3. Run `bazel run @rules_bazel_integration_test//tools:update_deleted_packages`
   to refresh `--deleted_packages` in `.bazelrc`.

For a module-graph failure (`MODULE.bazel` tag resolution, `bazel_dep`,
`use_repo`), create a standalone sub-workspace:

1. Create `tests/integration/<workspace>/` alongside `lock_dependency/` with its
   own `MODULE.bazel`, a copy of the root `.bazelversion`, and a target that
   forces evaluation of the broken extension or repository. Module extensions
   are lazy, so verify that building the target triggers it.
2. Add `<workspace>` to `_WORKSPACE_FILES` in `tests/integration/BUILD.bazel`
   (which generates `<workspace>_test`), and add
   `("<case>", "<workspace>", "<target>", "<stderr-fragment>")` to `_CASES` or
   `_RUN_CASES`.
3. List the workspace's four `bazel-*` symlinks (`bin`, `out`, `testlogs`,
   `<workspace>`) in `.bazelignore`. CI's `lint` job checks both these
   `.bazelignore` entries and the `.bazelversion` copy.
4. Run `bazel run @rules_bazel_integration_test//tools:update_deleted_packages`
   to refresh `--deleted_packages` in `.bazelrc`.

Choose a short, stable stderr substring rather than a full sentence so minor
OpenTofu or rule wording changes do not break the assertion.

## Follow-ups

`_RUN_CASES` covers the no-backend ephemeral-state refusal on `deploy.apply`
(`ephemeral_state_refused`). `run_directory_test` covers
`allow_ephemeral_state = True` and direct launcher execution.
`module_dependency_<version>_test` covers no-backend local state (step 6) and a
`backend "local"` without `allow_ephemeral_state`, both refused while its path
is relative and applied once it points outside `bazel-out` (step 7; the
remaining `backend "local"` cases live in `TestRunner_LocalBackend_Gate`).

Still untested in Bazel-in-Bazel: a remote backend or `cloud` block reaching
`tofu`, and hard `fail()` paths in `tf_providers` for a root tag that is
declared twice.
