# rules_tofu

[![CI](https://github.com/benbirt/rules_tofu/actions/workflows/ci.yml/badge.svg)](https://github.com/benbirt/rules_tofu/actions/workflows/ci.yml)
[![BCR](https://img.shields.io/badge/dynamic/json?url=https%3A%2F%2Fbcr.bazel.build%2Fmodules%2Frules_tofu%2Fmetadata.json&query=%24.versions%5B-1%3A%5D&label=BCR&color=43a047&logo=bazel)](https://registry.bazel.build/modules/rules_tofu)
[![Release](https://img.shields.io/github/v/release/benbirt/rules_tofu?logo=github)](https://github.com/benbirt/rules_tofu/releases)
[![API docs](https://img.shields.io/badge/docs-API-43a047?logo=bazel)](https://registry.bazel.build/modules/rules_tofu/latest/docs)
[![Bazel ≥ 8.5](https://img.shields.io/badge/bazel-%E2%89%A5%208.5-43a047)](https://bazel.build)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Bazel rules for [OpenTofu](https://opentofu.org/) and Terraform-compatible
configuration. `bazel build` runs `tofu init` and `tofu validate` offline as a
cached action; `bazel test` checks formatting and runs `tofu test`; `bazel run`
plans, applies and destroys. Bazel downloads and pins the `tofu` binary,
provider plugins and external modules by digest or commit, so `tofu init` never
contacts the network.

Requires Bazel 8.5 or later on Linux or macOS. You do not need `tofu` installed
because Bazel downloads it; OpenTofu 1.12 is the oldest supported release.

## Quick start

**1. Add `rules_tofu` to `MODULE.bazel`.** Take the version from the
[Bazel Central Registry](https://registry.bazel.build/modules/rules_tofu) or
[GitHub releases](https://github.com/benbirt/rules_tofu/releases):

```python
# MODULE.bazel
bazel_dep(name = "rules_tofu", version = "X.Y.Z")

tofu = use_extension("@rules_tofu//toolchain:extensions.bzl", "tofu")
# tofu.version(version = "1.12.0")
use_repo(tofu, "tofu_toolchains")
```

Leave out the `tofu` extension block to use the default OpenTofu version, which
`rules_tofu` registers automatically. See [The `tofu` binary](#the-tofu-binary).

**2. Write a configuration file and declare targets.**

```hcl
# hello/main.tf
variable "name" {
  type = string
}

output "greeting" {
  value = "Hello, ${var.name}!"
}
```

```python
# hello/BUILD.bazel
load("@rules_tofu//tf:defs.bzl", "tf_deploy", "tf_library")

tf_library(
    name = "greet_lib",
    srcs = ["main.tf"],
)

tf_deploy(
    name = "hello",
    allow_ephemeral_state = True,
    vars = {"name": "world"},
    deps = [":greet_lib"],
)
```

Without a remote `backend` or `cloud` block, `apply` and `destroy` refuse to run
unless `allow_ephemeral_state = True` is set. Real deploys declare a remote
backend in `srcs`; see [State and backends](#state-and-backends). Configurations
that use providers declare them in `MODULE.bazel` and list them in `providers`;
see [Providers](#providers).

**3. Build, test and run.**

```sh
bazel build //...
bazel test //...
bazel run //hello:hello.plan
bazel run //hello:hello.apply     # prompts; pass `-- -auto-approve` to skip
bazel run //hello:hello.destroy
```

[`examples/hello`](examples/hello) has this setup alongside `tf_test` targets.

## Rules

Load `tf_library`, `tf_deploy` and `tf_test` from `@rules_tofu//tf:defs.bzl`.
Full rule and module-extension reference is in the
[API docs](https://registry.bazel.build/modules/rules_tofu/latest/docs).

### `tf_library`

A reusable bundle of configuration files with no variable values and no `.plan`,
`.apply` or `.destroy` targets.

| Attribute | Purpose |
|-----------|---------|
| `srcs` | `.tf`, `.tf.json`, `.tofu`, `.tofu.json`, `.tftpl` and `.hcl` files. `.tofu` and `.tofu.json` shadow `.tf` and `.tf.json` of the same name; see [`examples/tofu_files`](examples/tofu_files). |
| `deps` | `tf_library` targets whose files, providers and `module_source` mappings propagate through this library. |
| `data` | Files placed in the work tree for `file()` and `templatefile()`. |
| `providers` | Providers required by `srcs`, as `@tf_providers//<namespace>/<name>`. |
| `module_source` | Address by which HCL `module` blocks call this library when it lives in another Bazel repository. See [Modules](#modules). |
| `module_version` | Version checked against callers' `version` constraints on a registry `module_source`. Defaults to the version of the Bazel module that holds the library when `module_source` is set; setting it without `module_source` is an error. |
| `fmt_test` | Emit `:name.fmt_check` when `srcs` is non-empty. Defaults to `True`. |

### `tf_deploy`

A root configuration with variable values bound. Its own Bazel package is the
root module and must contain at least one configuration file, either in `srcs`
or in a same-package `tf_library`. Libraries in other packages sit at their own
paths in the work tree and are reached only through `module` blocks; without a
configuration file in the deploy's package, `bazel build` fails.

| Attribute | Purpose |
|-----------|---------|
| `srcs` | Deploy-local configuration files, typically `backend` and `provider` blocks, with the same allowed extensions as `tf_library.srcs`. |
| `deps` | `tf_library` targets included in the work tree. |
| `vars` | String dictionary of variable values written to `rules_tofu.auto.tfvars.json`. See [Variables and data files](#variables-and-data-files). |
| `var_files` | `.tfvars` or `.tfvars.json` files passed via `-var-file`, including generated outputs. |
| `data` | Files placed in the work tree for `file()` and `templatefile()`. |
| `providers` | Providers required directly by `srcs`, combined with providers from `deps`. |
| `allow_ephemeral_state` | Permit `apply` and `destroy` when state lives under `bazel-bin`. Defaults to `False`; see [State and backends](#state-and-backends). |
| `fmt_test` | Emit `:name.fmt_check` when `srcs` is non-empty. Defaults to `True`. |

### `tf_test`

Runs `tofu test` over a module's test files as a Bazel test. See
[Tests](#tests).

| Attribute | Purpose |
|-----------|---------|
| `srcs` | `.tftest.hcl`, `.tofutest.hcl`, `.tftest.json` and `.tofutest.json` files in one directory at or below `module`'s package. `.tofutest.*` shadows `.tftest.*`. |
| `module` | The `tf_library` or `tf_deploy` under test. |
| `deps` | Helper `tf_library` targets loaded by `run { module { ... } }` blocks. |
| `providers` | Providers needed beyond those of `module` and `deps`. |
| `var_files` | `.tfvars` or `.tfvars.json` files passed via `-var-file`. |
| `data` | Files placed in the work tree. |
| `allow_network` | Allow network access during tests instead of blocking it in the sandbox. Defaults to `False`. |
| `fmt_test` | Emit `:name.fmt_check`. Defaults to `True`. |

### Generated targets

| Target | Rule | Command | Behaviour |
|--------|------|---------|-----------|
| `:name` | `tf_library`, `tf_deploy` | `bazel build` | Materialises the work tree and runs `tofu init -backend=false && tofu validate`. |
| `:name` | `tf_test` | `bazel test` | Runs `tofu test` offline; `bazel build` validates the module and test files. |
| `:name.fmt_check` | all | `bazel test` | Checks `tofu fmt` formatting of `.tf`, `.tofu`, `.tftest.hcl` and `.tofutest.hcl` files in `srcs`, including generated and external files. |
| `:name.fmt` | all | `bazel run` | Reformats main-repository `.tf`, `.tofu`, `.tftest.hcl` and `.tofutest.hcl` source files in `srcs` in place. |
| `:name.plan` | `tf_deploy` | `bazel run` | Runs `tofu plan`, saving the plan file to `<state-dir>/tfplan`. |
| `:name.apply` | `tf_deploy` | `bazel run` | Runs a fresh `tofu apply` without reading `tfplan`. Prompts unless `-- -auto-approve` is passed. |
| `:name.destroy` | `tf_deploy` | `bazel run` | Runs `tofu destroy`. Prompts unless `-- -auto-approve` is passed. |

Each deploy launcher, such as `bazel-bin/<pkg>/<name>.apply.sh`, can also be run
directly from any working directory. It locates its runfiles and state directory
relative to its own path, so do not copy or symlink the script elsewhere. Extra
arguments after `--`, or passed directly to the `.sh` script, are forwarded to
`tofu` in the deploy's shared run directory. See [How it works](#how-it-works).

`:name.fmt` and `:name.fmt_check` are emitted when `srcs` is non-empty. They act
only on the files in `srcs`, including those in subdirectories; other files in
the package are left alone.

## Providers

BUILD files name providers as `@tf_providers//<namespace>/<name>` in lower case
and without a version; for example, `PostHog/posthog` becomes
`@tf_providers//posthog/posthog`. Only providers required by built targets are
downloaded. `tofu init` runs offline against them, while providers still call
their own APIs during `plan`, `apply`, `destroy` and networked tests.

Archives come from `registry.opentofu.org` unless `url_template` is set. That
includes OpenTofu's builds of `hashicorp/` providers, whose checksums differ
from `releases.hashicorp.com`; see [Restrictions](#restrictions) for registry
scope.

Versions come from `.terraform.lock.hcl` via `from_lock` or from `provider` tags
in `MODULE.bazel`. The two can be mixed across providers, but pinning the same
provider in both the lock and a root tag fails when that provider is fetched.
Remove the tag, or run `bazel run @tf_providers//:pin` to rewrite the lock
without root-tagged providers. Full tag reference is in the
[API docs](https://registry.bazel.build/modules/rules_tofu/latest/docs).

### Pinning with a lock file

```python
# MODULE.bazel
tf_providers = use_extension("@rules_tofu//tf/providers:extensions.bzl", "tf_providers")
tf_providers.from_lock(lock_file = "//:.terraform.lock.hcl")
use_repo(tf_providers, "tf_providers")
```

`lock_file` is required and must name a `.terraform.lock.hcl` file in the main
repository, such as `"//:.terraform.lock.hcl"` or
`"//infra:.terraform.lock.hcl"`. Its directory must be a Bazel package with a
`BUILD.bazel` or `BUILD` file, which may be empty. Write `lock_file` as a string
literal so Dependabot can find the lock; see
[Keeping the lock up to date](#keeping-the-lock-up-to-date).

Add `@tf_providers//<namespace>/<name>` to `providers` on the targets requiring
each provider, then write the lock:

```sh
bazel run @tf_providers//:pin
```

`pin` collects `required_providers` across the workspace's `tf_library`,
`tf_deploy` and `tf_test` targets and their external `deps`, leaving out
directories of a fetched module outside `modules/` that nothing calls, such as
its `examples/`. It resolves the newest version satisfying every constraint and
writes `.terraform.lock.hcl`; conflicting constraints fail without changing the
lock.

By default, `pin` locks the five platforms that `from_lock` resolves:
`linux_amd64`, `linux_arm64`, `darwin_amd64`, `darwin_arm64` and
`windows_amd64`. If a provider is not published for all five, pass `-platform`
after `--` to replace that list:

```sh
bazel run @tf_providers//:pin -- -platform=linux_amd64 -platform=darwin_arm64
```

Without exact constraints, re-running `pin` upgrades every provider to the
newest allowed version. Reusable libraries declare `>=` floors under
[OpenTofu's provider version guidance](https://opentofu.org/docs/language/providers/requirements/#best-practices-for-provider-versions),
so put exact pins in a `versions.tf` target to hold versions steady across `pin`
runs; see [`examples/lock`](examples/lock):

```hcl
# versions.tf
terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "6.2.0"
    }
  }
}
```

Only the root module's `from_lock` is read; `from_lock` in a `bazel_dep` is
ignored. Lock files written by `terraform` against `registry.terraform.io` are
rejected until regenerated with `pin`. When `required_providers` drifts from the
lock, `bazel build` fails and points to `pin`.

#### Keeping the lock up to date

Locked providers record nothing in `MODULE.bazel.lock`, so updating
`versions.tf` and `.terraform.lock.hcl` needs no Bazel command:

- Renovate's built-in `terraform` manager updates `versions.tf` and
  `.terraform.lock.hcl` together.
- Dependabot requires `package-ecosystem: "opentofu"` rather than `"terraform"`,
  `directory` set to the lock's directory, and a `.tf` file such as
  `versions.tf` alongside the lock.

Dependabot's `bazel` ecosystem updates `MODULE.bazel` without cloning the
repository. It fetches `MODULE.bazel`, the root Bazel files, and the files
`MODULE.bazel` names in recognised attributes such as `lock_file`, together with
the `BUILD.bazel` or `BUILD` file beside each. It then regenerates
`MODULE.bazel.lock`, which runs `from_lock`. Dependabot finds the lock only when
`lock_file = "..."` is a string literal in `MODULE.bazel` rather than built from
a variable.

### Pinning with tags

Use `tf_providers.provider` to pin zip SHA256 digests in `MODULE.bazel` or to
download from a mirror; see [`examples/aws`](examples/aws),
[`examples/gcp`](examples/gcp) and [`examples/posthog`](examples/posthog):

```python
tf_providers = use_extension("@rules_tofu//tf/providers:extensions.bzl", "tf_providers")
tf_providers.provider(
    source = "hashicorp/aws",
    version = "5.70.0",
    sha256 = {  # optional when downloading from registry.opentofu.org
        "linux_amd64": "157ec2424b34ba3a2208ce39763528131d5600daffd72e8f47b08cbd7feebbd4",
        "darwin_arm64": "59894534729ed071176f794d25d1cb7aecdcffd6825fbb6100e815ba6e12ec10",
    },
)
use_repo(tf_providers, "tf_providers")
```

- Without `sha256`, digests for whichever of the five default platforms the
  provider is published for are recorded in `MODULE.bazel.lock`; commit it and
  run CI with `--lockfile_mode=error`.
- With `sha256`, every listed platform is downloaded when the provider is
  fetched, so list each `<os>_<arch>` you build on; accepted keys are in the
  [API docs](https://registry.bazel.build/modules/rules_tofu/latest/docs).
  Without `url_template`, a digest that differs from `registry.opentofu.org`
  fails the fetch and reports the registry's value, which you can also query
  with
  `curl -s https://registry.opentofu.org/v1/providers/hashicorp/aws/5.70.0/download/linux/amd64 | jq -r .shasum`.
- `url_template` downloads from a mirror using `{namespace}`, `{name}`,
  `{version}`, `{os}` and `{arch}` placeholders, and requires `sha256`. See the
  [API docs](https://registry.bazel.build/modules/rules_tofu/latest/docs) for an
  example.

Across Bazel modules, the root module's lock or tag wins; otherwise an agreed
version or the highest plain numeric version is selected. Non-numeric version
and digest conflict rules are in the
[API docs](https://registry.bazel.build/modules/rules_tofu/latest/docs).

## Modules

`tofu init` never fetches modules. Every called module is a `tf_library` in
`deps`; `rules_tofu` generates the module manifest at build time, installs it at
`.terraform/modules/modules.json`, and runs `tofu init -get=false`. See
[How it works](#how-it-works).

### In the workspace

Workspace files are materialised at their workspace-relative paths, so files in
the same package see each other directly. Call modules in other workspace
packages by relative paths starting with `./` or `../`; OpenTofu parses any
other prefix as a registry address:

```hcl
module "dns" {
  source = "../../infra/dns"
}
```

### From a registry, git or an archive

Declare external modules in `MODULE.bazel`, add their `@tf_modules` labels to
`deps`, and call them in HCL by the same `source`; see
[`examples/modules/label`](examples/modules/label),
[`examples/modules/stack`](examples/modules/stack) and
[`examples/modules/git`](examples/modules/git):

```python
# MODULE.bazel
tf_modules = use_extension("@rules_tofu//tf/modules:extensions.bzl", "tf_modules")
tf_modules.module(source = "terraform-aws-modules/vpc/aws", version = "5.8.1")
tf_modules.module(name = "acme_vpc", source = "git::https://github.com/acme/infra.git//vpc?ref=v1.2.0")
tf_modules.module(name = "acme_dns", source = "https://example.com/dns-1.0.0.tar.gz", sha256 = "...")
use_repo(tf_modules, "tf_modules")
```

```python
# BUILD.bazel
tf_library(
    name = "network",
    srcs = ["main.tf"],
    deps = ["@tf_modules//terraform-aws-modules/vpc/aws"],
)
```

```hcl
module "vpc" {
  source  = "terraform-aws-modules/vpc/aws"
  version = "~> 5.8"
}

module "endpoints" {
  source = "terraform-aws-modules/vpc/aws//modules/vpc-endpoints"
}
```

- Registry modules take `source = "[<host>/]<namespace>/<name>/<system>"`
  without `//<subdir>`. They are labelled
  `@tf_modules//<namespace>/<name>/<system>` in lower case, or
  `@tf_modules//example.com/acme/vpc/aws` for a non-default `modules.v1` host.
  `version` must be an exact `X.Y.Z` rather than a constraint or a prerelease
  such as `1.2.0-rc1`; fetch prereleases via git or an archive. Call submodules
  in HCL with `//<subdir>` while depending on the parent module's label.
- Git modules take `git::https://...`, `github.com/<owner>/<repo>` or `git@...`,
  with optional `//<subdir>` and `?ref=`. They are labelled
  `@tf_modules//<name>` and require `git` on `PATH`.
- Archive modules take `https://...`, `http://...` or `file://...` archive URLs,
  with optional `//<subdir>` or `?archive=`. They are labelled
  `@tf_modules//<name>` and require `sha256`. Full URL forms, archive
  extensions, `name` rules and cross-module resolution are in the
  [API docs](https://registry.bazel.build/modules/rules_tofu/latest/docs).

Every fetch is pinned:

- Registry modules record their download location and git commit or archive
  `sha256` in `MODULE.bazel.lock`.
- Git modules fetch `commit` when set on the tag, or resolve `?ref=`, defaulting
  to `HEAD`, on first fetch and record the commit in `MODULE.bazel.lock`. Commit
  IDs in `commit` or `?ref=` must be full 40- or 64-character lowercase hex
  hashes.
- Archive modules verify `sha256`.

Unpacking removes upstream Bazel files, `.git/`, `.terraform/`, lock files and
auto-loaded `.tfvars` files. Symlinks to directories, missing targets or paths
outside the tree are also removed, and `//<subdir>` cannot be a symlink. Git
submodules are not fetched; see [Known gaps](#known-gaps).

Each generated `tf_library` carries the providers required by the module's root,
its `modules/` subdirectories, and directories it calls itself. If only your
configuration calls a `//<subdir>` outside `modules/`, add that submodule's
providers to your target's `providers`; `bazel build` names any missing labels,
and `pin` still locks them. External modules called transitively must also be
declared with `tf_modules.module`.

#### Keeping modules up to date

Renovate updates registry modules in `MODULE.bazel` with a regex manager paired
with its `terraform` manager so HCL constraints update in the same PR:

```json
{
  "customManagers": [
    {
      "customType": "regex",
      "managerFilePatterns": ["/(^|/)MODULE\\.bazel$/"],
      "matchStrings": [
        "tf_modules\\.module\\(\\s*source\\s*=\\s*\"(?<depName>[^\"/]+/[^\"/]+/[^\"/]+)\",\\s*version\\s*=\\s*\"(?<currentValue>[^\"]+)\"",
        "tf_modules\\.module\\(\\s*version\\s*=\\s*\"(?<currentValue>[^\"]+)\",\\s*source\\s*=\\s*\"(?<depName>[^\"/]+/[^\"/]+/[^\"/]+)\""
      ],
      "datasourceTemplate": "terraform-module",
      "registryUrlTemplate": "https://registry.opentofu.org"
    }
  ],
  "packageRules": [
    { "matchDatasources": ["terraform-module"], "groupName": "OpenTofu modules", "registryUrls": ["https://registry.opentofu.org"] }
  ]
}
```

Registry commits live in `MODULE.bazel.lock`, so run
`bazel mod deps --lockfile_mode=update` after a version bump before CI runs with
`--lockfile_mode=error`. Self-hosted Renovate can run this in
`postUpgradeTasks`; otherwise run it on the PR branch. Dependabot does not
update `tf_modules.module` tags.

### From other Bazel repositories

A `tf_library` in another Bazel repository, such as a `bazel_dep`, cannot be
called by a relative path. Give each exported library its own Bazel package and
set `module_source` to the address HCL callers write:

```python
# BUILD.bazel in @acme_infra//vpc
tf_library(
    name = "vpc",
    srcs = glob(["**/*.tf"]),
    module_source = "git::https://github.com/acme/infra.git//vpc?ref=v1.2.0",
    visibility = ["//visibility:public"],
)

# BUILD.bazel in caller
tf_library(
    name = "network",
    srcs = ["main.tf"],
    deps = ["@acme_infra//vpc"],
)
```

A registry `module_source` also needs `module_version` set to a version that
callers' `version` constraints accept. When `module_source` is set,
`module_version` defaults to the version of the Bazel module that holds the
library; setting `module_version` without `module_source` is an error.

Every `module` block's `source` must be a literal string. Non-local calls match
a `tf_library` in `deps` by `module_source`, either exact or leaving the
shortest `//<subdir>` suffix. Direct `module_source` dependencies unused by the
target's own configuration fail `bazel build`. Full call-matching rules are in
the [API docs](https://registry.bazel.build/modules/rules_tofu/latest/docs).

## Variables and data files

`vars` sets string-valued variables inline. `var_files` passes `.tfvars` or
`.tfvars.json` files, including generated outputs, via `-var-file`. `data`
symlinks files at workspace-relative paths for `file()` and `templatefile()`:

```python
genrule(
    name = "secrets",
    outs = ["secrets.tfvars.json"],
    cmd = "$(location //tools:fetch-secrets) > $@",
    tools = ["//tools:fetch-secrets"],
)

tf_deploy(
    name = "prod",
    data = ["//config:tls_cert.pem"],  # file("config/tls_cert.pem")
    vars = {"region": "us-east-1"},
    var_files = [":secrets"],
    deps = [":network"],
)
```

`tf_deploy` writes `vars` to `rules_tofu.auto.tfvars.json` in its package
directory. OpenTofu converts string values such as `"3"` or `"true"` when a
variable is typed `number` or `bool`, while untyped variables receive the string
as written. Put lists, maps and objects in `var_files` instead. Because
`tofu validate` ignores variable values, a type mismatch in `vars` surfaces at
`plan` rather than `bazel build`.

Variable keys must not overlap across `vars` and `var_files` on a `tf_deploy`;
`bazel build` fails on duplicates.

## State and backends

A deploy managing real infrastructure should declare a remote `backend "..." {}`
or `cloud {}` block in `srcs`. The runner never rewrites backend blocks; `tofu`
resolves them from the deploy's run directory,
`bazel-bin/<pkg>/<name>.rules_tofu-state/work/<pkg>`.

State lands inside `bazel-bin` in two cases:

- No `backend` or `cloud` block: state is stored at
  `bazel-bin/<pkg>/<name>.rules_tofu-state/terraform.tfstate`.
- `backend "local"` without literal absolute paths outside `bazel-out` for both
  `path` and `workspace_dir`. Those attributes default to `terraform.tfstate`
  and `terraform.tfstate.d` in the run directory. Even with paths outside
  `bazel-out`, local backend state remains single-machine.

State under `bazel-bin` is machine-local, forks across build configurations and
is deleted by `bazel clean`, so `apply` and `destroy` refuse to run against it
by default. `plan` is never gated. Set `allow_ephemeral_state = True` only for
demos, throwaway sandboxes or state-bucket bootstrap. After bootstrapping a
state bucket, add the remote `backend` block, run `tofu init -migrate-state`,
and remove `allow_ephemeral_state`.

## Tests

`tf_test` runs `tofu test` offline against vendored providers and modules:

```python
tf_test(
    name = "network_test",
    srcs = glob(["tests/*.tftest.hcl"]),
    module = ":network",
    deps = ["//network/tests/fixtures:subnet_names"],
)
```

```hcl
# tests/subnets.tftest.hcl
run "names" {
  module {
    source = "./tests/fixtures" # relative to :network's package directory
  }
}

run "plans_a_subnet_per_name" {
  command = plan
  variables {
    subnet_names = run.names.names
  }
  assert {
    condition     = length(aws_subnet.this) == length(run.names.names)
    error_message = "expected one subnet per name"
  }
}
```

- Tests can live in a sub-package, and `module` may be an `alias`; see
  [`examples/hello/tests`](examples/hello/tests). When `srcs` sits in a
  subdirectory, `tofu test` still reads any test files in `module`'s root
  directory.
- Helper modules in `run` blocks come from `deps`, reached by a path relative to
  `module`'s package directory or by `module_source`.
- Available providers combine `module`, `deps` and `providers`. `mock_provider`
  still needs the provider package for its schema. If `module`'s root directory
  has no configuration files, `tofu init` installs no providers; add a file
  declaring `required_providers` there when helper modules need providers.
- `tf_test` initialises with `-backend=false`, ignoring any backend on a tested
  `tf_deploy`. The deploy's `vars` and `var_files` still apply;
  `tf_test.var_files` may override them, and `variables` blocks in test files
  take precedence over both.
- `run` blocks apply by default. `tf_test` adds Bazel's `block-network` tag so
  accidental cloud calls fail in the sandbox while plans and mocked applies run
  offline. Setting `allow_network = True` switches to `external` and
  `requires-network`, and disables test caching.
- `tofu-test.json` is written to Bazel's undeclared test outputs. If a `run`
  block's destroy fails, `tofu test` writes `errored_test.tfstate` and exits 0;
  `tf_test` fails the test and copies that file into undeclared outputs. Pass
  extra flags with `--test_arg=-verbose`.

## The `tofu` binary

Bazel downloads the OpenTofu release for the execution platform, verifies its
SHA256 against [`toolchain/versions.bzl`](toolchain/versions.bzl), and registers
it as `@rules_tofu//toolchain:toolchain_type`. One version serves the whole
build. A single `tofu.version` tag in the root module wins, and declaring more
than one in the root module is an error. Without a root tag, any dependencies
that tag a version must agree on it; with no tags anywhere, the default in
`toolchain/versions.bzl` is used.

To use a release that is not in `toolchain/versions.bzl`, either open a PR that
adds it with `bazel run //tools:update_versions`, or register your own binary.
`update_versions` verifies the cosign signature on `SHA256SUMS`.

### Bringing your own binary

`opentofu_toolchain` wraps any OpenTofu 1.12+ executable, whether vendored,
patched or built with `rules_go`. Older releases lack flags such as
`tofu test -json-into`, and `rules_tofu` does not check the binary's version.
Root-module `register_toolchains` calls take precedence over the default:

```python
# BUILD.bazel; register in MODULE.bazel with `register_toolchains("//:tofu_toolchain")`
load("@rules_tofu//toolchain:toolchain.bzl", "opentofu_toolchain")

opentofu_toolchain(
    name = "tofu_impl",
    binary = ":tofu",
    version = "1.12.0",
    platform_key = "linux_amd64",  # <os>_<arch> matching provider packages
)

toolchain(
    name = "tofu_toolchain",
    toolchain = ":tofu_impl",
    toolchain_type = "@rules_tofu//toolchain:toolchain_type",
)
```

When building `github.com/opentofu/opentofu/cmd/tofu` with `rules_go`, set
`pure = "on"` and `x_defs = {"github.com/opentofu/opentofu/version.dev": "no"}`;
without `x_defs`, `tofu version` reports `-dev` and fails `required_version`
constraints. Go and `go_deps` ignore `replace` directives in non-root modules,
so your root `go.mod` must also repeat OpenTofu's `replace` of
`github.com/hashicorp/hcl/v2` with `github.com/opentofu/hcl/v2`. Because a
`bazel_dep` cannot apply that `replace` and a source build pulls roughly three
hundred Go modules into every consumer's graph, `rules_tofu` ships verified
downloads instead.

## How it works

`tf_library` and `tf_deploy` assemble a work tree in
`bazel-bin/<pkg>/<name>.work/`, symlinking main-repository sources, `data` and
`var_files` at workspace-relative paths and external repositories under
`external/<repository>/`. `tf_deploy` writes `vars` to
`rules_tofu.auto.tfvars.json`. Both rules symlink exec-platform provider
packages into `bazel-bin/<pkg>/<name>.plugins/` and write the module manifest to
`bazel-bin/<pkg>/<name>.modules.json`. `bazel build` copies the work tree into a
scratch directory, installs that manifest at
`<pkg>/.terraform/modules/modules.json`, and runs
`tofu init -backend=false -get=false -plugin-dir=... && tofu validate` as a
cached action.

`.plan`, `.apply` and `.destroy` locate their runfiles and state directory from
`$0` and mirror the work tree as symlinks into
`bazel-bin/<pkg>/<name>.rules_tofu-state/work/`. They run `tofu` inside `<pkg>`
there so `path.module`, `path.cwd`, `.terraform/` and relative local state stay
consistent across runs until `bazel clean`. If a leftover regular file in the
state tree collides with a configuration path, the runner fails rather than
overwriting it. Each run installs `<name>.modules.json` at
`.terraform/modules/modules.json`, pins `TF_DATA_DIR` to `.terraform`, and
unsets `TF_PLUGIN_CACHE_DIR`. It then runs
`tofu init -input=false -get=false -reconfigure -plugin-dir=...` to re-read the
backend block without migrating state, followed by the command with `-var-file`
flags.

`tf_test` validates `module` and `srcs` during `bazel build`. During
`bazel test`, it copies the work tree into `$TEST_TMPDIR`, installs
`<name>.modules.json`, and runs `tofu init -backend=false` followed by
`tofu test`. Both phases pass `-test-directory` when `srcs` is in a
subdirectory of `module`'s package.

## Restrictions

- OpenTofu ignores dot-prefixed filenames in `srcs`.
- Test files belong in `tf_test.srcs`, not `tf_library` or `tf_deploy`.
- `.terraform.lock.hcl` is rejected in `srcs` and `data`; pass lock files via
  `tf_providers.from_lock`.
- `rules_tofu.auto.tfvars.json` is reserved in a `tf_deploy` package for `vars`.
- `data` rejects `terraform.tfvars`, `terraform.tfvars.json`, `*.auto.tfvars`
  and `*.auto.tfvars.json` in any directory because OpenTofu auto-loads them
  outside the duplicate-key check; pass variable files via `var_files`.
- `.plan`, `.apply` and `.destroy` take no lock on their shared run directory;
  run one command at a time per deploy.
- Providers must belong to `registry.opentofu.org` or built-in
  `terraform.io/builtin/*`, even when `url_template` downloads archives from a
  mirror.
- `tf_modules.module` does not support S3, GCS, Bitbucket, Mercurial or
  `X-Terraform-Get` module sources.
- `tf_providers.from_lock` and `tf_modules.module` compile Go helpers with
  `rules_go`'s host Go SDK and fetch Go module dependencies via `GOPROXY`;
  honoured proxy variables are listed in the
  [API docs](https://registry.bazel.build/modules/rules_tofu/latest/docs).
  Workspaces without a Go SDK use `rules_tofu`'s pinned SDK, while workspaces or
  earlier `bazel_dep`s that register one must select at least the `go` version
  in [`go.mod`](go.mod).

## Non-goals

- Running `terraform`. `rules_tofu` drives `tofu` only. Most Terraform `.tf`
  configurations, providers and modules work unchanged under OpenTofu; OpenTofu's
  [migration guide](https://opentofu.org/docs/intro/migration/) lists the
  exceptions, such as HCP Terraform and Stacks. `rules_tofu` bakes
  `registry.opentofu.org` into its plugin directory layout and lock files, so
  wrapping a `terraform` binary in `opentofu_toolchain` fails at provider
  resolution.
- Building OpenTofu from source. See [The `tofu` binary](#the-tofu-binary).

## Known gaps

- Validation and runtime launchers both use exec-platform provider binaries, so
  the build host must match the run host.
- Provider and toolchain downloads support Windows, but generated launcher
  scripts require `bash`.
- `tf_modules` does not fetch git submodules, including for git-backed registry
  modules. If the module's root directory is a git submodule or its own `module`
  calls enter one, fetching fails and names the submodule. A submodule reached
  only via `file()` or an external `//<subdir>` call is omitted at fetch time
  and fails when read or called.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for the repository layout, test suites
and release process. Released under the [MIT License](LICENSE).
