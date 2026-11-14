# Integration tests

Bazel-in-Bazel tests: each runs a nested Bazel on a sub-workspace that
uses rules_tofu as a downstream module would. The negative ones assert
that misuses of the rules fail with the error messages the rules promise;
the positive ones drive whole workflows (pinning, fetching modules,
formatting, running a deploy's launchers).

## Layout

```
tests/integration/
├── BUILD.bazel              the tests: one for broken_workspace's cases, one
│                            per case with a sub-workspace of its own, and
│                            the positive tests
├── cases.bzl                the line format of a case
├── running_bazel.bzl        the nested Bazel's version: the running one
├── nested_bazel.sh          sourced by every driver: the nested `bazel`
├── assert_bazel_fails.sh    driver of the negative tests: runs each case's
│                            nested bazel (build or run) and asserts a
│                            non-zero exit and a stderr fragment
├── *_test.sh                drivers of the positive tests, one each
├── broken_workspace/        negative cases that share one MODULE.bazel
│   ├── cases/<case>/        one package per case; BUILD.bazel lists them
│   └── lock/                the workspace's `from_lock` lock and versions.tf
├── broken_module/           negative: the break is the module graph itself
│                            (two root tofu.version tags)
├── lock_dependency/         negative: a dependency's `from_lock` is ignored
├── custom_toolchain/        the root module registers its own `tofu`
│                            toolchain; its stand-in binary fails with a
│                            message that proves it was selected
├── lock_update/             positive: `pin`, then a bot's lock update
├── module_dependency/       positive: modules from other Bazel modules
├── tf_modules/              positive: the tf_modules extension
├── run_directory/           positive: a deploy's launchers, run for real
└── fmt/                     positive: `.fmt` and `.fmt_check`
```

Every sub-workspace has its own `MODULE.bazel` and a `.bazelversion`
identical to the root's. A case's files are whatever its failure needs;
its `BUILD.bazel` says what it misuses.

`lock_dependency/` holds a root module without a lock and `lib_module`, a
dependency whose `from_lock` names a file that isn't a lock at all. The
dependency's tag must be ignored and its lock never read, so building the
dependency's library fails only for want of the provider, which the root
doesn't supply.

`lock_update/` and its driver `lock_update_test.sh` are a positive
test: a workspace's first `@tf_providers//:pin`, with the provider already
named in a BUILD file, then a provider update made the way Dependabot or
Renovate makes it (`versions.tf` and `infra/.terraform.lock.hcl` only, from
`bumped.lock.hcl`), which must build with `--lockfile_mode=error` and leave
`MODULE.bazel.lock` unchanged. `bazel mod tidy` must then also succeed, and
change nothing, in a copy holding only the files Dependabot's `bazel`
updater fetches.

`module_dependency/` and its driver `module_dependency_test.sh` are
another: `//app` calls a `tf_library` from the `network_module` Bazel module
by its `module_source`, and that library calls one from `dns_module` by
its own. Everything must build, each external library on its own too,
`//app:app.plan` must plan, and the deploy's `tf_test` must run and leave
its JSON log. The generated module manifest is tofu's internal format, so
there is one `module_dependency_<version>_test` per tofu version in
`toolchain/versions.bzl`: with the fmt tests, the place where every pinned
version is exercised, a `tf_test` included.

`tf_modules/` and its driver `tf_modules_test.sh` cover the `tf_modules`
extension: the driver turns `fixtures/network` into a git repository and
`fixtures/dns` into an archive, so both are fetched offline, beside
`cloudposse/label/null` from the OpenTofu registry. `//app` plans with all
three and two submodules, without naming the provider the git module
requires; without the provider of the submodule outside `modules/` in its
`providers`, it fails naming the label to add; the commits are recorded in
`MODULE.bazel.lock`; moving the git tag changes nothing, even after
`bazel clean --expunge`; and a module calling an undeclared one fails when
fetched, naming the tag to add.

`run_directory/` and its driver `run_directory_test.sh` run a deploy's
launchers for real. `//app:deploy` declares a `backend "local"` with a
relative `path` and reads a `data` file through `path.module`.
`bazel run //app:deploy.apply` must create its resource with the state in
the deploy's run directory under `bazel-bin` and nothing written to the
launcher's runfiles; the built `deploy.destroy.sh`, run directly by a
relative path from another directory without `bazel run`'s environment,
must find that state and destroy the resource; and `//app:deploy.plan` must
then plan against the same state.

`fmt/` and its driver `fmt_test.sh` cover the `.fmt` and `.fmt_check`
targets, which act on the files in `srcs` by name: a target whose `srcs`
are all in a subdirectory passes its check; a library's unformatted `.tf`
and `.tofu` files in subdirectories fail its check, each named, as a
`tf_test`'s `.tftest.hcl` fails that target's; `.fmt` rewrites exactly those
and leaves the package's file outside `srcs` byte-identical; and a target
without `srcs` has neither target. The driver ends by building the whole
workspace, so the fixture stays a shape the rules accept, and by running
its test of an empty root module. Which extensions
`tofu fmt` accepts by name is the binary's to decide, so there is one
`fmt_<version>_test` per pinned tofu version.

`broken_workspace` shares one `MODULE.bazel` across all of its cases, so a
case whose failure lives in the module graph cannot go there — it gets a
sub-workspace of its own. `BUILD.bazel` names the workspace per case row
and globs each workspace's files once.

All of `broken_workspace`'s cases run as one test, `broken_workspace_test`,
against a single nested Bazel: starting a server and fetching and analysing
rules_tofu's dependencies costs far more than any case, so after the first
they are incremental builds of a few seconds. The driver runs every case
even after a failure, prints the nested log of each case that fails, and
names them all at the end. A case with its own sub-workspace is its own
test.

The child workspace packages are excluded from the parent Bazel via
the `--deleted_packages` flag in `.bazelrc` (the "deleted packages"
trick recommended by `rules_bazel_integration_test`), so the outer
Bazel never analyses the sub-workspaces' targets, the intentionally
failing ones among them, when expanding `//...`.

## Case lists

`BUILD.bazel` holds two lists, distinguished only by the nested Bazel
command the driver runs:

- `_CASES` — build-mode cases. The failure comes from loading,
  analysis, or one of the actions the rules emit, so `bazel build` on
  the case target is enough to surface it.
- `_RUN_CASES` — run-mode cases, driven with `bazel run`. The case
  target builds cleanly and the refusal comes from the launcher process
  a `tf_deploy` generates. These are the only tests that exercise the
  path from a BUILD attribute through the generated launcher to a runner
  flag; the runner's own Go unit tests pass flags directly and cannot
  see that plumbing.

Each row becomes a `name|command|target|fragment` line of the driver's
`CASES` environment variable.

## Running

```sh
bazel test //tests/integration:all
```

Each test spawns a nested Bazel. For a negative case it materialises the
case's work tree and runs `tofu init/validate` (or the dupcheck action),
or — for a run-mode case — launches the deploy's runner, and the driver
checks the failure mode. A positive test's driver runs the steps its
header lists.

The nested Bazel is the same version as the Bazel running the tests
(`@running_bazel` in the root `MODULE.bazel`), whatever a sub-workspace's
`.bazelversion` says: that file is for running `bazel` inside the
sub-workspace by hand. So to test on another Bazel, run the tests with it:

```sh
USE_BAZEL_VERSION=8.5.0 bazel test //tests/integration:all
```

Every `bazel_integration_test` passes `RUNNING_BAZEL_VERSION` in its `env`
(`_ENV` in `BUILD.bazel`), and `nested_bazel.sh` fails the test if the
nested Bazel is another version.

To run a selection of `broken_workspace`'s cases, name them:

```sh
bazel test //tests/integration:broken_workspace_test \
    --test_env=ONLY_CASES=stale_lock,missing_provider --test_output=all
```

## Adding a case

1. Create `broken_workspace/cases/<case>/` with a `BUILD.bazel` that
   misuses the rules, plus any fixture files the failure mode needs.

   If the misuse is in the *module graph* rather than in a BUILD file —
   extension tag resolution, a bad `bazel_dep`, a `use_repo` mismatch —
   it cannot live in `broken_workspace`, whose single `MODULE.bazel` is
   shared by every case. Add a new sub-workspace alongside it (see
   `broken_module/`) with its own `MODULE.bazel`, a `.bazelversion`
   byte-identical to the root one, and a case package holding whatever
   target forces the broken part to be evaluated. Module extensions are
   lazy, so check empirically that the target you pick actually triggers
   the failure. Register the new workspace in `_WORKSPACE_FILES`, and
   list its four `bazel-*` symlinks in the root `.bazelignore` (CI's lint
   job checks both this and the `.bazelversion`).
2. Add a row to `tests/integration/BUILD.bazel`:
   `("<case>", "<workspace>", "//cases/<case>:<target>", "<stderr-fragment>")`.
   Put it in `_CASES` if `bazel build` on the target fails, or in
   `_RUN_CASES` if the target builds and only `bazel run` fails. A
   `broken_workspace` row joins `broken_workspace_test`; a row for
   another workspace becomes a `<case>_test` of its own.
3. Execute `bazel run @rules_bazel_integration_test//tools:update_deleted_packages`
   to add the new package to `--deleted_packages` in the root `.bazelrc`.

Pick a stderr fragment short and stable enough not to break on tofu /
rule-error wording tweaks; avoid quoting full sentences.

## Follow-ups

All core build-time negative failure modes are covered, as is one
runtime mode in both its forms: the ephemeral-state refusal on
`bazel run :deploy.apply` when the deploy has not opted in and has no
backend, or a `backend "local"` that keeps state under `bazel-bin`. The
opt-in direction, and a launcher run outside `bazel run`, are covered by
`run_directory_test`.

Still untested end-to-end: a remote backend reaching tofu, and
`tf_providers` module-resolution errors. These can be addressed in
future testing passes if needed.
