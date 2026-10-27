# rules_tofu 🌱

[![CI](https://github.com/benbirt/rules_tofu/actions/workflows/ci.yml/badge.svg)](https://github.com/benbirt/rules_tofu/actions/workflows/ci.yml)
[![BCR](https://img.shields.io/badge/dynamic/json?url=https%3A%2F%2Fbcr.bazel.build%2Fmodules%2Frules_tofu%2Fmetadata.json&query=%24.versions%5B-1%3A%5D&label=BCR&color=43a047&logo=bazel)](https://registry.bazel.build/modules/rules_tofu)
[![Release](https://img.shields.io/github/v/release/benbirt/rules_tofu?logo=github)](https://github.com/benbirt/rules_tofu/releases)
[![Bazel ≥ 8.5](https://img.shields.io/badge/bazel-%E2%89%A5%208.5-43a047)](https://bazel.build)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Bazel rules for [OpenTofu](https://opentofu.org/) and Terraform-compatible
configuration. Your `.tf` files become ordinary build targets: `bazel build`
validates them, `bazel test` checks their formatting, and `bazel run` plans,
applies and destroys them, with providers and the `tofu` binary vendored by
Bazel.

## Why

- **Build = validate.** `bazel build //...` runs `tofu init` and
  `tofu validate` over every library and deploy, offline and cached.
- **Hermetic and reproducible.** Bazel downloads the `tofu` binary and
  every provider plugin, checked against pinned digests. No `tofu init`
  hitting the network, no `.terraform/` directories.
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

Both rules load from `@rules_tofu//tf:defs.bzl`.

### `tf_library`

A reusable bundle of configuration files. It carries no variable values and
is not runnable.

| Attribute   | Purpose |
|-------------|---------|
| `srcs`      | `.tf`, `.tf.json`, `.tftpl` and `.hcl` files. |
| `deps`      | Other `tf_library` targets this one composes with. |
| `data`      | Extra files placed in the work tree for `file()` and `templatefile()`. |
| `providers` | Providers it uses, as `@tf_providers//<namespace>/<name>`. Propagate to every deploy that depends on it. |
| `fmt_test`  | Generate the `.fmt_check` test (default `True`). |

### `tf_deploy`

A root invocation: binds variable values to libraries and runs them.

| Attribute               | Purpose |
|-------------------------|---------|
| `srcs`                  | Deploy-local files, typically `backend` and provider blocks. |
| `deps`                  | `tf_library` targets to deploy. |
| `vars`                  | Variable values, written to `rules_tofu.auto.tfvars.json`. |
| `var_files`             | `.tfvars` or `.tfvars.json` files, including generated ones. Passed via `-var-file`. |
| `data`                  | As for `tf_library`. |
| `providers`             | Providers the deploy's own `srcs` use. |
| `allow_ephemeral_state` | Let `apply` and `destroy` run with no backend. See [State](#state-and-backends). |
| `fmt_test`              | As for `tf_library`. |

### Generated targets

| Target             | Rule    | Command      | Does |
|--------------------|---------|--------------|------|
| `:name`            | both    | `bazel build`| Materialises the work tree and runs `tofu init -backend=false && tofu validate`. |
| `:name.fmt_check`  | both    | `bazel test` | Fails if `srcs` are not `tofu fmt` clean. |
| `:name.fmt`        | both    | `bazel run`  | Reformats `srcs` in place. |
| `:name.plan`       | deploy  | `bazel run`  | `tofu plan`, saving the plan to the state directory. |
| `:name.apply`      | deploy  | `bazel run`  | `tofu apply`. Prompts unless `-- --auto-approve`. |
| `:name.destroy`    | deploy  | `bazel run`  | `tofu destroy`. Prompts unless `-- --auto-approve`. |

There is no separate `:name.validate` target. Building the rule is the
validation contract.

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
and locked.

### Pinning with a lock file

```python
# MODULE.bazel
tf_providers = use_extension("@rules_tofu//tf/providers:extensions.bzl", "tf_providers")
tf_providers.from_lock()  # reads //:.terraform.lock.hcl; pass lock = "..." to move it
use_repo(tf_providers, "tf_providers")
```

Pin exact versions in a `versions.tf` beside the lock. That file belongs to
a `tf_library` like any other `.tf` source. Libraries that use the provider
name it in `providers` and declare at most a `>=` floor:

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

Write or update the lock with:

```sh
bazel run @tf_providers//:pin
```

`pin` collects the `required_providers` of every `tf_library` and
`tf_deploy` in the workspace and locks for `linux_amd64`, `linux_arm64`,
`darwin_amd64`, `darwin_arm64` and `windows_amd64` (`-platform=<os>_<arch>`
chooses others). If the constraints conflict it lists them by directory and
leaves the lock unchanged. Without a `versions.tf`, `pin` locks the newest
version every library accepts, and re-running it upgrades everything.

A few rules follow from OpenTofu's own model:

- One lock per workspace, and one version of each provider across the build.
- Only the root module's lock counts. A `from_lock` in a Bazel module you
  depend on is ignored: libraries constrain, the root decides.
- If the lock no longer satisfies a library's constraints, building that
  library fails and tells you to run `pin`.

Each upgrade is a reviewable two-line change: the pin in `versions.tf` and
the matching lock entry. [`examples/lock`](examples/lock) is a complete
example.

#### Keeping the lock up to date 🤖

- **Renovate** works with its default `terraform` manager. With a
  `versions.tf` it updates the pin and the lock in one PR.
- **Dependabot** needs `package-ecosystem: "opentofu"` (not `terraform`),
  with `directory` set to the lock's directory. It only reads `.tf` files in
  that directory, so a `versions.tf` there is required in practice.

Locked providers record nothing in `MODULE.bazel.lock`, so these PRs are
complete as they are and pass with `--lockfile_mode=error`.

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

## State and backends 🔒

A deploy that manages real infrastructure should declare a `backend "..." {}`
or `cloud {}` block in its own `srcs`. The runner then defers to that
backend entirely.

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

## Module source paths

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

## How it works

`tf_deploy` symlinks every transitive `.tf` file, plus `data` and
`var_files`, into `bazel-bin/<pkg>/<name>.work/` at its workspace-relative
path, and writes `vars` to `rules_tofu.auto.tfvars.json` there. Building
the target runs `tofu init -backend=false && tofu validate` over that tree
as a cached action.

At run time a small Go launcher enters the work tree and runs
`tofu init -input=false -plugin-dir=<vendored providers>`, which disables
plugin auto-installation, then the requested command. `var_files` are
passed to every command with `-var-file`.

## Restrictions

- `srcs` accepts `.tf`, `.tf.json`, `.tftpl` and `.hcl` only. Variable
  files are rejected there: values come through `vars` or `var_files`.
- `apply` and `destroy` need a backend block or `allow_ephemeral_state`.
- Files from other Bazel modules can't be part of a deploy. The runner has
  no workspace-relative path for them that local module addressing can
  reach.

## Known gaps

- The build host must match the run host: validation and the launcher both
  use the exec-platform provider binaries.
- Windows: downloads work, but the launcher is bash-only.
- OpenTofu is a downloaded binary, not built from source.
- No generated API docs on releases yet.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for the repository layout, test
suites and release process. Released under the [MIT License](LICENSE).
