# rules_tofu 🌱

[![CI](https://github.com/benbirt/rules_tofu/actions/workflows/ci.yml/badge.svg)](https://github.com/benbirt/rules_tofu/actions/workflows/ci.yml)
[![BCR](https://img.shields.io/badge/dynamic/json?url=https%3A%2F%2Fbcr.bazel.build%2Fmodules%2Frules_tofu%2Fmetadata.json&query=%24.versions%5B-1%3A%5D&label=BCR&color=43a047&logo=bazel)](https://registry.bazel.build/modules/rules_tofu)
[![Release](https://img.shields.io/github/v/release/benbirt/rules_tofu?logo=github)](https://github.com/benbirt/rules_tofu/releases)
[![API docs](https://img.shields.io/badge/docs-API-43a047?logo=bazel)](https://registry.bazel.build/modules/rules_tofu/latest/docs)
[![Bazel ≥ 8.5](https://img.shields.io/badge/bazel-%E2%89%A5%208.5-43a047)](https://bazel.build)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Bazel rules for [OpenTofu](https://opentofu.org/) and Terraform-compatible
configuration. Your `.tf` files become ordinary build targets: `bazel build`
validates them, `bazel test` checks their formatting and runs their
`tofu test` tests, and `bazel run` plans, applies and destroys them, with
providers and the `tofu` binary vendored by Bazel.

## Why

- **Build = validate.** `bazel build //...` runs `tofu init` and
  `tofu validate` over every library and deploy, offline and cached.
- **Hermetic and reproducible.** Bazel downloads the `tofu` binary, every
  provider plugin and every module, each pinned by a digest or commit. No
  `tofu init` hitting the network, no `.terraform/` directories.
- **Composable.** `tf_library` is to `.tf` files what `cc_library` is to
  C++: reusable, dependency-aware, and not runnable on its own. `tf_deploy`
  is the root that binds variables and runs.
- **Plays well with bots.** Provider versions live in a standard
  `.terraform.lock.hcl` that Renovate and Dependabot already understand.
- **Generated inputs.** Variable files can be the output of any other Bazel
  rule, such as a `genrule` that fetches secrets.

## Quick start 🚀

Requires Bazel 8.5 or later on Linux or macOS.

**1. Add the module.** Get the latest version from the
[Bazel Central Registry](https://registry.bazel.build/modules/rules_tofu)
or the [release notes](https://github.com/benbirt/rules_tofu/releases).

```python
# MODULE.bazel
bazel_dep(name = "rules_tofu", version = "X.Y.Z")

tofu = use_extension("@rules_tofu//toolchain:extensions.bzl", "tofu")
# tofu.version(version = "1.8.5")  # optional; a default is pinned
use_repo(tofu, "tofu_toolchains")
```

One `tofu` version serves the whole build. A `tofu.version` tag in the root
module wins; otherwise your dependencies must agree on one, and with no tags
at all the version pinned in [`toolchain/versions.bzl`](toolchain/versions.bzl)
is used.

**2. Declare targets.**

```python
# BUILD.bazel
load("@rules_tofu//tf:defs.bzl", "tf_deploy", "tf_library")

tf_library(
    name = "network",
    srcs = ["network.tf", "outputs.tf"],
)

tf_deploy(
    name = "prod",
    srcs = ["backend.tf"],  # backend and provider config local to this deploy
    deps = [":network"],
    vars = {
        "region": "us-east-1",
        "env": "prod",
    },
)
```

**3. Build, test, run.**

```sh
bazel build //...                      # tofu init + tofu validate, cached
bazel test //...                       # tofu fmt checks
bazel run //path/to:prod.plan
bazel run //path/to:prod.apply         # prompts; add `-- --auto-approve` to skip
bazel run //path/to:prod.destroy
```

[`examples/hello`](examples/hello) is this quick start, runnable, with no
providers. To use providers, continue with [Providers](#providers).

## The rules

The rules load from `@rules_tofu//tf:defs.bzl`. The full API reference,
including the module extensions' tags, is on the
[Bazel Central Registry](https://registry.bazel.build/modules/rules_tofu/latest/docs).

### `tf_library`

A reusable bundle of configuration files. A library carries no variable
values and is not runnable on its own.

| Attribute   | Purpose |
|-------------|---------|
| `srcs`      | `.tf`, `.tf.json`, `.tofu`, `.tofu.json`, `.tftpl` and `.hcl` files. A `.tofu` file is read in place of the `.tf` file of the same name ([`examples/tofu_files`](examples/tofu_files)). |
| `deps`      | Dependency `tf_library` targets. |
| `data`      | Extra files placed in the work tree for `file()` and `templatefile()`. |
| `providers` | Providers the library's files require, as `@tf_providers//<namespace>/<name>`. |
| `module_source` | The address by which other modules call this one, e.g. `git::https://example.com/acme/network.git?ref=v1.0.0`. See [Modules](#modules-). |
| `module_version` | This module's version, required when `module_source` is a registry address. Defaults to the version of the Bazel module that holds the library. |
| `fmt_test`  | Generate the `.fmt_check` test (default `True`). |

### `tf_deploy`

A root configuration: the deploy's own files plus dependency libraries,
with variable values bound. This is the target `bazel run` plans, applies
and destroys.

| Attribute               | Purpose |
|-------------------------|---------|
| `srcs`                  | The deploy's own `.tf`, `.tf.json`, `.tofu`, `.tofu.json`, `.tftpl` and `.hcl` files, typically `backend` and provider blocks. |
| `deps`                  | Dependency `tf_library` targets. |
| `vars`                  | Variable values, as a dict of name to value. |
| `var_files`             | `.tfvars` or `.tfvars.json` files, including files generated by other rules. |
| `data`                  | Extra files placed in the work tree for `file()` and `templatefile()`. |
| `providers`             | Providers the deploy's own `srcs` require, as `@tf_providers//<namespace>/<name>`. |
| `allow_ephemeral_state` | Let `apply` and `destroy` run with no backend. See [State](#state-and-backends). |
| `fmt_test`              | Generate the `.fmt_check` test (default `True`). |

### `tf_test`

A module's OpenTofu tests, the `.tftest.hcl` files `tofu test` runs, as a
Bazel test. See [Tests](#tests-).

| Attribute       | Purpose |
|-----------------|---------|
| `srcs`          | The test files: `.tftest.hcl`, `.tofutest.hcl`, `.tftest.json` and `.tofutest.json`, all in one directory. |
| `module`        | The `tf_library` or `tf_deploy` under test. |
| `deps`          | Helper `tf_library` targets that `run { module { ... } }` blocks load. |
| `providers`     | Providers the tests need beyond the module's and helpers' own. |
| `var_files`     | `.tfvars` or `.tfvars.json` files passed with `-var-file`. |
| `data`          | Extra files placed in the work tree. |
| `allow_network` | Let the tests reach the network (default `False`; see [Tests](#tests-)). |
| `fmt_test`      | Generate the `.fmt_check` test (default `True`). |

### Generated targets

| Target             | Rule    | Command      | Does |
|--------------------|---------|--------------|------|
| `:name`            | library, deploy | `bazel build`| Materialises the work tree and runs `tofu init -backend=false && tofu validate`. |
| `:name`            | test    | `bazel test` | Runs `tofu test` offline against the module under test; `bazel build` validates it with the test files. |
| `:name.fmt_check`  | all     | `bazel test` | Fails if the `.tf`, `.tofu`, `.tftest.hcl` and `.tofutest.hcl` files in `srcs` are not `tofu fmt` clean. |
| `:name.fmt`        | all     | `bazel run`  | Reformats those files in place. |
| `:name.plan`       | deploy  | `bazel run`  | `tofu plan`, saving the plan to the state directory. |
| `:name.apply`      | deploy  | `bazel run`  | `tofu apply`. Prompts for confirmation unless the command is followed by `-- --auto-approve`. |
| `:name.destroy`    | deploy  | `bazel run`  | `tofu destroy`. Prompts for confirmation unless the command is followed by `-- --auto-approve`. |

There is no separate `:name.validate` target. If the rule builds
successfully, the configuration is valid.

`:name.fmt` and `:name.fmt_check` exist only when `srcs` is non-empty. Both
name the files in `srcs` one by one, so a file in a subdirectory of the
package counts and a file in the package that isn't in `srcs` is left
alone. `.fmt` rewrites source files of the main repository; a generated
file in `srcs` is checked but never rewritten.

## Providers 📦

Providers are downloaded by Bazel and handed to the rules that use them.
BUILD files name a provider as `@tf_providers//<namespace>/<name>`, never
with a version, and only the providers a build uses are fetched.

Versions come from one of two places:

- **A lock file** (recommended): a standard `.terraform.lock.hcl` plus a
  `versions.tf`, exactly as in a plain OpenTofu project. Renovate and
  Dependabot keep it current.
- **Tags in `MODULE.bazel`**: for what a lock can't express, such as
  digests you pin yourself or a download mirror.

The two can be mixed, but a provider can't be both tagged in the root module
and locked: if one is, builds that use it fail, naming the conflict, until
`bazel run @tf_providers//:pin` has rewritten the lock without it (tagged
providers are left out) or the tag is removed. Every tag and attribute is
described in the
[`tf_providers` reference](https://registry.bazel.build/modules/rules_tofu/latest/docs/tf/providers/extensions.bzl).

### Pinning with a lock file

```python
# MODULE.bazel
tf_providers = use_extension("@rules_tofu//tf/providers:extensions.bzl", "tf_providers")
tf_providers.from_lock()  # reads //:.terraform.lock.hcl; pass lock = "..." for another path
use_repo(tf_providers, "tf_providers")
```

Name each provider in the `providers` of the libraries and deploys that use
it, then write the lock with:

```sh
bazel run @tf_providers//:pin
```

`pin` locks every provider that any `tf_library` or `tf_deploy` in the
workspace requires, or any library they depend on (from `tf_modules` or
another Bazel repository: the module itself, its `modules/`, the submodules
they call with `//<subdir>`, and what those call locally, but not its
examples), for `linux_amd64`, `linux_arm64`, `darwin_amd64`, `darwin_arm64`
and `windows_amd64` (`-platform=<os>_<arch>` chooses others).
Each provider is locked at the newest version that satisfies every
`required_providers` constraint in the workspace. If the constraints
conflict, `pin` lists them by directory and leaves the lock unchanged.

**Recommended: pin exact versions in a `versions.tf` beside the lock.** This
is optional, and follows
[OpenTofu's advice](https://opentofu.org/docs/language/providers/requirements/#best-practices-for-provider-versions):
the root configuration pins exact versions, and reusable modules declare at
most a `>=` floor. Here the whole workspace is the root configuration.
`versions.tf` belongs to a `tf_library` like any other `.tf` source:

```hcl
# versions.tf: the only place versions are pinned
terraform {
  required_providers {
    aws = { source = "hashicorp/aws", version = "6.2.0" }
  }
}
```

```python
# BUILD.bazel
tf_library(
    name = "versions",
    srcs = ["versions.tf"],
    providers = ["@tf_providers//hashicorp/aws"],
)

tf_library(
    name = "network",
    srcs = ["main.tf"],  # may declare `version = ">= 6.0"`
    providers = ["@tf_providers//hashicorp/aws"],
)
```

With a `versions.tf`, each upgrade is a reviewable two-line change: the pin
in `versions.tf` and the matching lock entry. Without one, the lock is the
only record of the versions in use, and re-running `pin` upgrades every
provider to the newest version the constraints allow.

A few rules follow from OpenTofu's own model:

- One lock per workspace, and one version of each provider across the build.
- Only the root module's lock counts. A `from_lock` in a Bazel module you
  depend on is ignored: libraries constrain, the root decides.
- If the lock no longer satisfies a library's constraints, building that
  library fails and tells you to run `pin`.
- `from_lock` reads the lock with a small Go helper compiled by the Go SDK
  rules_go selects for the host. A workspace that configures no Go SDK uses
  the one rules_tofu pins; one that registers its own (or whose earlier
  `bazel_dep` does) must select a Go at least as new as the `go` directive in
  rules_tofu's [`go.mod`](go.mod), or the helper fails to build, saying so.

[`examples/lock`](examples/lock) is a complete example.

#### Keeping the lock up to date 🤖

- **Renovate** works with its default `terraform` manager. With a
  `versions.tf` Renovate updates the pin and the lock in one PR; without one
  only the lock changes.
- **Dependabot** needs `package-ecosystem: "opentofu"` (not `terraform`),
  with `directory` set to the lock's directory. Dependabot only reads `.tf`
  files in that directory, so a `versions.tf` there is required in practice.

### Pinning with tags

Use a `provider` tag when you want to pin digests yourself or download from
a mirror:

```python
tf_providers = use_extension("@rules_tofu//tf/providers:extensions.bzl", "tf_providers")
tf_providers.provider(
    source = "hashicorp/aws",
    version = "5.70.0",
    sha256 = {  # optional: omit to trust the registry's digests
        "linux_amd64":  "157ec2424b34ba3a2208ce39763528131d5600daffd72e8f47b08cbd7feebbd4",
        "linux_arm64":  "47eba28f2c35d71d73dd2d0c2a09c89bdcc99136a5484904b59cc9c299f1a5aa",
        "darwin_amd64": "cad9a818e27739e0dcfb78c9170ce5e1b4619d48be7ec529618198633ec5bb63",
        "darwin_arm64": "59894534729ed071176f794d25d1cb7aecdcffd6825fbb6100e815ba6e12ec10",
    },
)
use_repo(tf_providers, "tf_providers")
```

- Without `sha256`, the OpenTofu registry's digests are used and recorded in
  `MODULE.bazel.lock`. Commit that file and run CI with
  `--lockfile_mode=error`.
- With `sha256`, a digest that disagrees with the registry fails the fetch
  and reports the registry's value. Get a digest with:

  ```sh
  curl -s https://registry.opentofu.org/v1/providers/hashicorp/aws/5.70.0/download/linux/amd64 | jq -r .shasum
  ```

- `url_template` downloads from a mirror and then requires `sha256`.
  Placeholders are `{namespace}`, `{name}`, `{version}`, `{os}` and
  `{arch}`:

  ```python
  tf_providers.provider(
      source = "PostHog/posthog",
      version = "1.0.24",
      url_template = "https://github.com/PostHog/terraform-provider-posthog/releases/download/v{version}/terraform-provider-{name}_{version}_{os}_{arch}.zip",
      sha256 = {...},
  )
  ```

Tagged providers are named like any other and are left out of the lock by
`pin`. When several Bazel modules tag the same provider, the root module's
tag or lock wins; otherwise the highest version is used. See
[`examples/aws`](examples/aws), [`examples/gcp`](examples/gcp) and
[`examples/posthog`](examples/posthog).

### How vendoring works

Providers come from the [OpenTofu registry](https://search.opentofu.org/)
unless a tag gives a `url_template`. These are the same builds that
`tofu init` installs; for `hashicorp/` providers they are OpenTofu's builds,
so their checksums differ from releases.hashicorp.com. Sources are
case-insensitive and named in lower case: `PostHog/posthog` becomes
`@tf_providers//posthog/posthog`.

The checks `tofu init` and `tofu validate` would make online run offline
during `bazel build`. The `plan`, `apply` and `destroy` targets use the
vendored plugins too, though the providers themselves still talk to their
own APIs. A `.terraform.lock.hcl` in `srcs` or `data` is rejected; use
`from_lock`.

## The `tofu` binary

Bazel downloads the OpenTofu release for the machine it runs on, checked
against the digest pinned in
[`toolchain/versions.bzl`](toolchain/versions.bzl), and registers it as a
toolchain. Those digests are copied from each release's `SHA256SUMS` only
after its cosign signature has been verified, by
`bazel run //tools:update_versions`. To use a release that isn't pinned yet,
open a PR that runs it, or bring your own binary.

### Bringing your own binary

`opentofu_toolchain` wraps any `tofu` executable: one you vendor, one you
patched, or one built from source with `rules_go`. Bazel consults the root
module's `register_toolchains` before those of its dependencies, so a
toolchain registered there is the one every rule uses:

```python
# BUILD.bazel
load("@rules_tofu//toolchain:toolchain.bzl", "opentofu_toolchain")

opentofu_toolchain(
    name = "tofu_impl",
    binary = ":tofu",              # a file, a go_binary, any executable
    version = "1.12.0",
    platform_key = "linux_amd64",  # <os>_<arch> of the provider builds it runs
)

toolchain(
    name = "tofu_toolchain",
    toolchain = ":tofu_impl",
    toolchain_type = "@rules_tofu//toolchain:toolchain_type",
)
```

```python
# MODULE.bazel
register_toolchains("//:tofu_toolchain")
```

For a source build, a `go_binary` of `github.com/opentofu/opentofu/cmd/tofu`
needs `pure = "on"` and
`x_defs = {"github.com/opentofu/opentofu/version.dev": "no"}`; without the
latter `tofu version` reports a `-dev` prerelease, which fails
`required_version` constraints. Expect OpenTofu's three hundred Go modules
to join your module graph, and its `replace` of `hashicorp/hcl/v2` with
`opentofu/hcl/v2` to need repeating in your own `go.mod`.

rules_tofu itself deliberately ships the download rather than a source
build. The pinned download is already hermetic, and a bundled source build
would put those modules and that `replace` into every downstream build.

## Variables and data files

`vars` is the common case. `var_files` adds `.tfvars` or `.tfvars.json`
files, which can be generated by other rules:

```python
genrule(
    name = "secrets",
    outs = ["secrets.tfvars.json"],
    cmd = "$(location //tools:fetch-secrets) > $@",
    tools = ["//tools:fetch-secrets"],
)

tf_deploy(
    name = "prod",
    deps = [":network"],
    vars = {"region": "us-east-1"},
    var_files = [":secrets"],
)
```

Variable keys must be unique across `vars` and every `var_files` entry. A
build action checks this, so an overlap fails `bazel build`, not the run.

`data` places arbitrary files in the work tree at their workspace-relative
path, for `file()` and `templatefile()`:

```python
tf_deploy(
    name = "prod",
    deps = [":network"],
    data = ["//config:tls_cert.pem"],  # file("config/tls_cert.pem")
)
```

## Tests 🧪

`tofu test` runs a module's `.tftest.hcl` files: `run` blocks that plan or
apply the module and check `assert` conditions, with mock providers and
helper modules as fixtures. `tf_test` runs them with `bazel test`, offline,
against the vendored providers:

```python
tf_test(
    name = "network_test",
    srcs = glob(["tests/*.tftest.hcl"]),
    module = ":network",                 # the tf_library or tf_deploy under test
    deps = ["//network/tests/fixtures:subnet_names"],  # a tf_library a run block loads
)
```

```hcl
# tests/subnets.tftest.hcl
run "names" {
  module {
    source = "./tests/fixtures"   # relative to the module's directory, network/
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

The test files go in `srcs` of the `tf_test`, never of a `tf_library` or
`tf_deploy`: tofu reads them from the root module's directory, so a test in
a library's `srcs` would be shipped to and loaded by every dependant. They
may be in the module's own directory or one subdirectory of it (`tests/`,
tofu's default, or another, passed as `-test-directory`), and a `tf_test`
holds one directory's files. The tests can live in a Bazel package of their
own, as [`examples/hello/tests`](examples/hello/tests) does. A `.tofutest.hcl`
is read in place of the `.tftest.hcl` of the same name.

**Helper modules.** A `run` block's `module { source = ... }` loads a module
in place of the one under test. Like every module, it comes from a
`tf_library` in `deps`: by a path relative to the module under test's
directory, or by its `module_source`, including a `@tf_modules//...` module.
Nothing is fetched; the module manifest records what tofu would have
installed.

**Providers.** The test's plugin tree holds the module's providers, the
helpers' and any in `providers`. A `mock_provider` still needs the real
provider package, for its schema, so a mocked test is offline but not
provider-free.

**Variables.** Testing a `tf_deploy`, its `vars` and `var_files` are in
force. The test's own `var_files` come after them, and a test file's
`variables` blocks win over both.

**Real infrastructure.** A `run` block applies by default, which creates real
resources. A `tf_test` carries Bazel's `block-network` tag, so plans and
mocked applies run in the sandbox while a run that reaches a cloud API fails
fast. Set `allow_network = True` for tests that must apply real
infrastructure: the test then carries `external` and `requires-network`, so
it is never cached.

**Output.** `tofu test`'s JSON log is in the test's undeclared outputs as
`tofu-test.json`. When a run's destroy fails, tofu writes the resources it
could not remove to `errored_test.tfstate` and still exits 0; the test fails
and keeps that file with its outputs. Extra arguments reach `tofu test`
through `--test_arg`, as in
`bazel test //network:network_test --test_arg=-verbose`.

## State and backends 🔒

A deploy that manages real infrastructure should declare a `backend "..." {}`
or `cloud {}` block in its own `srcs`. The runner then defers to that
backend entirely.

A `backend "local"` block is honoured only when its `path` (and
`workspace_dir`, if set) is an absolute, literal path: tofu resolves a
relative one against its working directory, which under `bazel run` is the
launcher's runfiles work tree, a per-target directory Bazel owns and prunes.
So `backend "local" {}` or a relative `path` is refused, for `plan` too, with
a message naming the alternatives.

Without one, state falls back to
`bazel-bin/<pkg>/<name>.rules_tofu-state/`. That location is disposable by
construction: `bazel clean` deletes it, a different build configuration
forks it, and it is local to one machine. Losing it orphans whatever was
applied. So `apply` and `destroy` **refuse to run** against a deploy with no
backend, naming the path and the fix. `plan` is never gated.

Where disposable state is the point, opt in:

```python
tf_deploy(
    name = "sandbox",
    allow_ephemeral_state = True,
    deps = [":lib"],
)
```

That suits demos, throwaway sandboxes and bootstrapping the bucket that will
hold real state. For the bootstrap case, add the backend block once the
bucket exists, migrate with `tofu init -migrate-state`, then drop the
attribute.

## Modules 🧩

`tofu init` never fetches a module. Every module a configuration calls is a
`tf_library` in `deps`, and rules_tofu records where each call's files are in
`.terraform/modules/modules.json`, as `tofu init` does after installing
modules, then runs `tofu init -get=false`.

### In the workspace

Every `.tf` file is materialised at its workspace-relative path, so
same-package files see each other bare and sibling packages are a relative
hop away:

```hcl
module "dns" {
  source = "../dns"
}
```

Terraform treats any `source` that doesn't start with `./` or `../` as a
registry address. To reach across the tree, climb to the workspace root and
back down, for example `../../infra/dns`.

### From a registry, git or an archive

Declare each module in `MODULE.bazel`, and call it from HCL by the same
`source`:

```python
# MODULE.bazel
tf_modules = use_extension("@rules_tofu//tf/modules:extensions.bzl", "tf_modules")
tf_modules.module(source = "terraform-aws-modules/vpc/aws", version = "5.8.1")
tf_modules.module(
    name = "acme_vpc",
    source = "git::https://github.com/acme/infra.git//vpc?ref=v1.2.0",
)
tf_modules.module(
    name = "acme_dns",
    source = "https://example.com/dns-1.0.0.tar.gz",
    sha256 = "...",
)
use_repo(tf_modules, "tf_modules")
```

```starlark
# BUILD.bazel
tf_library(
    name = "network",
    srcs = ["main.tf"],
    deps = [
        "@tf_modules//terraform-aws-modules/vpc/aws",
        "@tf_modules//acme_vpc",
    ],
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

module "acme_vpc" {
  source = "git::https://github.com/acme/infra.git//vpc?ref=v1.2.0"
}
```

A registry module is labelled `@tf_modules//<namespace>/<name>/<system>`,
and needs an exact `version`, which a call's `version` constraint must
accept. A git or archive module is labelled by its `name`. Submodules are
called with `//<subdir>` and come with the module's label.

Each module is pinned, so a tag that moves later changes nothing:

| Source | Pinned by |
|--------|-----------|
| Registry address | The commit (or archive digest) the registry gives for the version, recorded in `MODULE.bazel.lock`. |
| `git::<url>` or `github.com/<owner>/<repo>` | `commit`, or the commit that `?ref=` names when first fetched, recorded in `MODULE.bazel.lock`. |
| Archive URL | `sha256`, which is required. |

git sources are fetched with `git`, which must be on the `PATH`.
Fetched modules are described by a small Go helper compiled, like
`from_lock`'s, by the host Go SDK rules_go selects, which must be at least as
new as the `go` directive in rules_tofu's [`go.mod`](go.mod) when the
workspace registers a Go SDK of its own.

A module's files are its own. A symlink to a file of the module is kept and
read as that file; a symlink to a directory, to nothing or to anything
outside the module is removed when the module is fetched, as are any Bazel
files it ships. A module can't therefore be declared by a `//<subdir>` that
is a symlink: the fetch fails, naming the directory to declare instead.

A module's `tf_library` carries the providers its `required_providers`
name, and its `modules/` submodules', so callers don't list them. A
submodule outside `modules/` that you call by `//<subdir>` is different: the
library is generated when the module is fetched, before anything calls it, so
its providers go in the caller's `providers`. The build fails naming the
`@tf_providers//<namespace>/<name>` to add when one is missing. The root
module still pins them all, with tags or in its lock, which `pin` writes to
cover them, those submodules included. A module that calls
other modules by address needs them declared too: fetching it fails
otherwise, naming the `tf_modules.module` to add. Each module has one
version per build. For a registry module the root module's tag wins, else
the highest version requested.

#### Keeping modules up to date 🤖

Renovate updates registry modules with a regex manager for `MODULE.bazel`.
Grouping it with Renovate's own `terraform` manager, which updates the
`version` in HCL, puts both in one PR:

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
    {
      "matchDatasources": ["terraform-module"],
      "groupName": "OpenTofu modules",
      "registryUrls": ["https://registry.opentofu.org"]
    }
  ]
}
```

A new version's commit is recorded in `MODULE.bazel.lock`, so the PR is
complete only once `bazel mod deps --lockfile_mode=update` has run, and a
build with `--lockfile_mode=error` fails until then. Self-hosted Renovate
can run it as a `postUpgradeTasks` command; otherwise, run it on the PR's
branch. Dependabot doesn't update `tf_modules.module` tags.

### From other Bazel repositories

A `tf_library` that another Bazel repository ships, for example through a
`bazel_dep`, can't be reached by a relative path. Such a library sets
`module_source`, the address that users outside Bazel would write, and
callers use that address:

```starlark
# BUILD.bazel in the repository that ships the module
tf_library(
    name = "vpc",
    srcs = glob(["**/*.tf"]),
    module_source = "git::https://github.com/acme/infra.git//vpc?ref=v1.2.0",
    visibility = ["//visibility:public"],
)
```

```starlark
# BUILD.bazel in the caller
tf_library(
    name = "network",
    srcs = ["main.tf"],
    deps = ["@acme_infra//vpc"],
)
```

For a registry address, the library also needs a version, which a call's
`version` constraint must accept: `module_version`, which defaults to the
version of the Bazel module that holds it, so a module released to the BCR
needn't repeat it. The library's directory is its package, so give each
such library a package of its own.

### How calls are matched

A call matches the library in `deps` (direct or transitive) whose
`module_source` is its `source`, or the one whose `module_source` plus
`//<subdir>` is, the longest such. `tf_modules` sets `module_source` to the
tag's `source`. Sources are read as OpenTofu reads them: a registry address
matches with or without the default `registry.opentofu.org/` host, and the
`github.com/<owner>/<repo>` shorthand matches the git URL it stands for. Other
addresses must match exactly, `?ref=` included. A `source` must be a literal
string.

The build fails when:

- a call's `source` matches no library in `deps`;
- a library in `deps` with `module_source` is called neither by the root's
  own configuration (its package's directories) nor by a relative `source`
  from there into the library's package, and is not in the root's package;
  a call from inside another library doesn't count, since that library
  lists its own `deps`;
- two libraries have the same `module_source`;
- a call's `version` constraint rejects the matching version;
- a module, the root's own or one it calls, requires a provider that nothing
  in `providers` supplies, directly or through `deps`; the error names the
  `@tf_providers//<namespace>/<name>` to add.

## How it works

`tf_deploy` symlinks every transitive `.tf` file, plus `data` and
`var_files`, into `bazel-bin/<pkg>/<name>.work/` at its workspace-relative
path, and writes `vars` to `rules_tofu.auto.tfvars.json` there. Building
the target runs `tofu init -backend=false && tofu validate` over that tree
as a cached action.

Files from other Bazel repositories go under `external/<repository>/` in the
work tree, and a generated `.terraform/modules/modules.json` records the
directory of every module call.

At run time a small Go launcher enters the work tree and runs
`tofu init -input=false -get=false -reconfigure -plugin-dir=<vendored providers>`,
which disables module and plugin installation and re-reads the backend block
rather than the copy a previous run saved, then the requested command.
`var_files` are passed to every command with `-var-file`. tofu runs with
`TF_DATA_DIR` set to the work tree's own `.terraform` and without any
`TF_PLUGIN_CACHE_DIR`, so a shell's settings can't move the module manifest
or bypass the vendored providers.

A `tf_test` builds the same work tree for the module under test, with the
test files and the helper libraries in `deps` added, validates it with the
test directory, and at test time copies it into `$TEST_TMPDIR`, runs the
same offline `tofu init` with `-backend=false` (`tofu test` keeps its state
in memory and ignores backends), then `tofu test`.

## Restrictions

- `srcs` accepts `.tf`, `.tf.json`, `.tofu`, `.tofu.json`, `.tftpl` and
  `.hcl` only. Variable files are rejected there: values come through
  `vars` or `var_files`. Test files are rejected too: they go in a
  `tf_test`, whose `srcs` must be in one directory.
- `apply` and `destroy` need a backend block or `allow_ephemeral_state`.
- A module call can't use a relative path to reach files in another Bazel
  repository; it uses the library's `module_source`.

## Non-goals

- **Running Terraform.** rules_tofu runs OpenTofu only. The vast majority
  of Terraform configuration runs under OpenTofu unchanged, providers and
  modules included, so for a `.tf` tree this rarely matters; OpenTofu's
  [migration guide](https://opentofu.org/docs/intro/migration/) lists the
  exceptions, mostly features tied to HashiCorp's own services such as HCP
  Terraform and Stacks. Driving the `terraform` binary is not planned: the
  two tools expand provider addresses to different registry hostnames, which
  rules_tofu bakes into the vendored plugin tree and the
  `.terraform.lock.hcl` it writes, so swapping the binary would be the
  smallest part of the work. Registering a `terraform` binary as described
  above is untested and breaks at the provider mirror.
- **Building OpenTofu from source.** See [The `tofu` binary](#the-tofu-binary).

## Known gaps

- The build host must match the run host: validation and the launcher both
  use the exec-platform provider binaries.
- Windows: downloads work, but the launcher is bash-only.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for the repository layout, test
suites and release process. Released under the [MIT License](LICENSE).
