# rules_tofu

Bazel rules for managing [OpenTofu](https://opentofu.org/) (and, by extension,
Terraform-compatible) configuration as first-class build targets.

`rules_tofu` exposes two rules, patterned after the archived
[rules_k8s](https://github.com/bazelbuild/rules_k8s):

- `tf_library` — a reusable bundle of `.tf` files plus its transitive
  `tf_library` deps. Analogous to `cc_library`. Not directly runnable
  and carries no variable values.
- `tf_deploy` — a *root* invocation that binds variable values to
  one or more `tf_library` targets. The macro automatically
  generates three runnable sub-targets: `:foo.plan`, `:foo.apply`, and
  `:foo.destroy`. Analogous to `cc_binary`.

## Quick start

Requires Bazel 8.5 or later, on Linux or macOS.

In your downstream repo's `MODULE.bazel` (for the latest version, see
[registry.bazel.build/modules/rules_tofu](https://registry.bazel.build/modules/rules_tofu)
or the snippet in the release notes):

```python
bazel_dep(name = "rules_tofu", version = "X.Y.Z")

tofu = use_extension("@rules_tofu//toolchain:extensions.bzl", "tofu")
# tofu.version(version = "1.8.5")  # optional; defaults to a pinned version
use_repo(tofu, "tofu_toolchains")
```

One OpenTofu toolchain is registered for the whole build, so all
`tofu.version` tags in the module graph resolve to a single version. A tag
in the root module (at most one) takes precedence. Otherwise, the version
that dependencies declare is used if they all agree; if they disagree, the
build fails and asks the root module to declare one. With no tags at all,
the version pinned in [`toolchain/versions.bzl`](toolchain/versions.bzl) is
used.

In a `BUILD.bazel`:

```python
load("@rules_tofu//tf:defs.bzl", "tf_library", "tf_deploy")

tf_library(
    name = "network",
    srcs = ["network.tf", "outputs.tf"],
)

tf_deploy(
    name = "prod",
    deps = [":network"],
    srcs = ["backend.tf"],            # provider + backend config local to this deploy
    vars = {
        "region": "us-east-1",
        "env":    "prod",
    },
)
```

To use providers, pin them in a `versions.tf` and a standard
`.terraform.lock.hcl` at the workspace root:

```python
# MODULE.bazel
tf_providers = use_extension("@rules_tofu//tf/providers:extensions.bzl", "tf_providers")
tf_providers.from_lock()
use_repo(tf_providers, "tf_providers")
```

```hcl
# versions.tf
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
```

Then write the lock with `bazel run @tf_providers//:pin`, and add
`providers = ["@tf_providers//hashicorp/aws"]` to the libraries that use the
provider. [Providers](#providers) explains the details.

### Providers

Providers are vendored by Bazel and passed to the rules that use them. BUILD
files name them as `@tf_providers//<namespace>/<name>`, never with a
version, and only the providers a build uses are downloaded.

By default, versions are declared as in a plain OpenTofu project: in `.tf`
files and a standard `.terraform.lock.hcl`, which Renovate and Dependabot
can keep up to date. Tags in `MODULE.bazel` cover what a lock file can't
express, such as a mirror or digests you pin yourself. The two can be mixed,
but a provider can't be both tagged in the root module and locked.

#### Pinning with a lock file

```python
tf_providers = use_extension("@rules_tofu//tf/providers:extensions.bzl", "tf_providers")
tf_providers.from_lock(lock = "//:.terraform.lock.hcl")  # the default
use_repo(tf_providers, "tf_providers")
```

```python
tf_library(
    name = "network",
    srcs = ["main.tf"],
    providers = ["@tf_providers//hashicorp/aws"],
)
```

The workspace has one lock, and each provider has one version across the
build. As in OpenTofu, where only the root configuration's lock is used, only
the root module's lock counts: a `from_lock` in a Bazel module you depend on
is ignored. Libraries declare version constraints, and the root module
decides the versions.

To write or update the lock:

```sh
bazel run @tf_providers//:pin
```

`pin` locks against the `required_providers` of every `tf_library` and
`tf_deploy` in the workspace, for `linux_amd64`, `linux_arm64`,
`darwin_amd64`, `darwin_arm64` and `windows_amd64` (pass
`-platform=<os>_<arch>` to choose others). If the version constraints
conflict, it lists them by directory and leaves the lock unchanged.

**Pin versions in a `versions.tf` beside the lock (strongly recommended).**
This follows OpenTofu's advice: the root configuration pins exact versions,
and reusable modules declare at most a `>=` floor. Here the whole workspace
is the root configuration:

```hcl
# versions.tf, beside .terraform.lock.hcl: the only place versions are pinned
terraform {
  required_providers {
    aws = { source = "hashicorp/aws", version = "6.2.0" }
  }
}
```

```hcl
# network/main.tf
terraform {
  required_providers {
    aws = { source = "hashicorp/aws", version = ">= 6.0" }  # floor optional
  }
}
```

Like any `.tf` file that `pin` reads, `versions.tf` belongs to a rule:

```python
tf_library(
    name = "versions",
    srcs = ["versions.tf"],
    providers = ["@tf_providers//hashicorp/aws"],
)
```

Run `pin` after changing a pin. Each upgrade is then a reviewable one-line
change, which the bots below make for you.

**Without a `versions.tf`**, `pin` locks the newest version that every
library accepts. The lock is then the only record of which version is in
use, and re-running `pin` upgrades every provider within the libraries'
constraints.

If the lock doesn't satisfy a library's version constraints (for example,
after a pin or a floor is raised without re-running `pin`), building that
library fails with a message to run `pin`. `examples/lock/` is a full
end-to-end example.

Keeping the lock up to date:

- **Renovate** works with its default `terraform` manager. With a
  `versions.tf`, it updates the pin and the lock in one PR. Without one, it
  proposes updates to the lock alone.
- **Dependabot** needs `package-ecosystem: "opentofu"` (not `terraform`),
  with `directory` set to the lock's directory. It only reads `.tf` files in
  that directory, so without a `versions.tf` it needs another file there that
  declares each provider; a floor is enough.

#### Pinning with tags

Use a `tf_providers.provider` tag for a provider that needs something a lock
file can't express: digests you pin yourself, or a mirror instead of the
OpenTofu registry.

```python
tf_providers = use_extension("@rules_tofu//tf/providers:extensions.bzl", "tf_providers")
tf_providers.provider(
    source = "hashicorp/aws",
    version = "5.70.0",
)
use_repo(tf_providers, "tf_providers")
```

A tagged provider is named like any other, as `@tf_providers//hashicorp/aws`.
`pin` leaves tagged providers out of the lock. When several modules tag the same provider, the root module's
tag or lock decides; otherwise the highest requested version is selected.

Without `sha256`, the OpenTofu registry's digests are used, and recorded in
`MODULE.bazel.lock`. Commit `MODULE.bazel.lock` to keep builds reproducible,
and run CI with `--lockfile_mode=error` so that a stale one fails the build.

To pin the digests yourself, give each platform you'll run on its `shasum`
from the registry's download endpoint:

```python
tf_providers.provider(
    source = "hashicorp/aws",
    version = "5.70.0",
    sha256 = {
        "linux_amd64":   "157ec2424b34ba3a2208ce39763528131d5600daffd72e8f47b08cbd7feebbd4",
        "linux_arm64":   "47eba28f2c35d71d73dd2d0c2a09c89bdcc99136a5484904b59cc9c299f1a5aa",
        "darwin_amd64":  "cad9a818e27739e0dcfb78c9170ce5e1b4619d48be7ec529618198633ec5bb63",
        "darwin_arm64":  "59894534729ed071176f794d25d1cb7aecdcffd6825fbb6100e815ba6e12ec10",
    },
)
```

```sh
curl -s https://registry.opentofu.org/v1/providers/hashicorp/aws/5.70.0/download/linux/amd64 | jq -r .shasum
```

If a pinned digest disagrees with the registry, the fetch fails and reports
the registry's value. To download from a mirror instead of the registry,
give the tag a `url_template`; it then needs `sha256`:

```python
tf_providers.provider(
    source = "PostHog/posthog",
    version = "1.0.24",
    url_template = "https://github.com/PostHog/terraform-provider-posthog/releases/download/v{version}/terraform-provider-{name}_{version}_{os}_{arch}.zip",
    sha256 = {...},
)
```

The placeholders are `{namespace}`, `{name}`, `{version}`, `{os}` and
`{arch}`. `examples/aws/`, `examples/gcp/` and `examples/posthog/` are full
end-to-end examples.

#### How providers are vendored

Either way, providers are downloaded from the
[OpenTofu registry](https://search.opentofu.org/) (unless a tag declares a
`url_template`): the same builds that `tofu init` installs. For
`hashicorp/` providers, these are OpenTofu's builds, not HashiCorp's, so
their checksums differ from those on releases.hashicorp.com. Provider
sources are case-insensitive, as in OpenTofu, and are named in lower case
(`PostHog/posthog` → `@tf_providers//posthog/posthog`).

A `tf_deploy` vendors its own `providers` plus those of its library `deps`.

The checks that `tofu init` and `tofu validate` would normally make online,
against the registry, run offline as part of `bazel build` and `bazel test`,
once Bazel has downloaded the providers. `:foo.plan` and `:foo.apply` also
use the vendored providers, though the providers themselves still contact
their own APIs (AWS, GCP and so on). A `.terraform.lock.hcl` listed in
`srcs` or `data` is rejected; use `from_lock` instead.

See `examples/hello/` for a minimal example of `tf_library` and `tf_deploy`
with no providers.

### Variable values and data files

Variable values can also come from `.tfvars.json` files — including files
generated by other Bazel rules:

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
    var_files = [":secrets"],   # keys in secrets.tfvars.json must not overlap with vars
)
```

The `data` attribute lets you place arbitrary files into the work tree so
that Terraform's `file()` function can read them at runtime:

```python
tf_deploy(
    name = "prod",
    deps = [":network"],
    vars = {"region": "us-east-1"},
    data = ["//config:tls_cert.pem"],  # available as file("config/tls_cert.pem") in .tf files
)
```

Files listed in `data` are symlinked into the work tree at their
workspace-relative short path (e.g. `config/tls_cert.pem`), making them
readable via Terraform's `file()` or `templatefile()` functions.

To run a deploy:

```sh
bazel run //path/to:prod.plan
bazel run //path/to:prod.apply             # prompts for approval
bazel run //path/to:prod.apply -- --auto-approve   # skip prompt
bazel run //path/to:prod.destroy           # prompts for approval
bazel run //path/to:prod.destroy -- --auto-approve # skip prompt
```

## How it works

At analysis time `tf_deploy` materializes every transitive `.tf`
file — plus any `var_files` — into a directory tree under
`bazel-bin/<pkg>/<name>.work/` via `ctx.actions.symlink`, preserving
each file's workspace-relative path. It also writes
`<name>.work/<pkg>/rules_tofu.auto.tfvars.json` from `vars = {...}`.

`bazel build :foo` (whether `:foo` is a `tf_library` or a
`tf_deploy`) materializes the work tree and runs
`tofu init -backend=false && tofu validate` as a build action, so
configuration syntax/reference errors fail the build (with action
caching). `bazel test //...` runs the `:foo.fmt_check` test targets;
validation does not live in a separate `:foo.validate` target — building
the rule itself is the validation contract.

At runtime the launcher invokes a small Go binary that changes into the
materialized work tree, runs
`tofu init -input=false -plugin-dir=<vendored>` (which "overrides all
default search paths for plugins, and prevents the automatic installation
of plugins"), then runs the requested command:

- **plan** — runs `tofu plan`, saving the plan artifact to
  `<state-dir>/tfplan`.
- **apply** — runs `tofu apply`, which plans and then prompts for
  approval. Pass `--auto-approve` to skip the prompt.
- **destroy** — runs `tofu destroy`, prompting for approval. Pass
  `--auto-approve` to skip the prompt.

`var_files` entries are passed to every command via `-var-file=<path>`.
A `bazel build` action checks for duplicate variable keys across `vars`
and all `var_files` entries — overlaps fail the build (with caching),
not the run.

Any deploy that manages real infrastructure is expected to declare a
`backend "..." {}` or `cloud {}` block in one of its own `.tf` files. When
one is present the runner defers to that backend entirely and passes no
local-state flags.

With no backend block, state falls back to the deploy target's
`$(RULEDIR)`, i.e. `bazel-bin/<package>/<name>.rules_tofu-state/`. That
location is disposable by construction: `bazel clean` deletes it, the path
embeds the build configuration (so `-c opt` or a different `--platforms`
forks to a separate state file), and it is machine-local, so two engineers
each get their own copy. Losing or forking it orphans whatever was applied.

So `:<name>.apply` and `:<name>.destroy` **refuse to run** against a deploy
with no backend block, naming the path and the fix. `:<name>.plan` is never
gated — planning against empty local state destroys nothing.

Where disposable state is the point, opt in explicitly:

```python
tf_deploy(
    name = "sandbox",
    allow_ephemeral_state = True,
    deps = [":lib"],
)
```

That is appropriate for demos and examples, throwaway sandboxes, and the
bootstrap case — using tofu to create the bucket that will later hold real
state. For bootstrapping, add the backend block once the bucket exists and
move the state into it with `tofu init -migrate-state`, then drop the
attribute.

## Module source paths

Each `.tf` is materialized at its workspace-relative path, so:

- Same-package files reference each other bare.
- Sibling-package libraries reference each other via relative paths:

```hcl
module "dns" {
  source = "../dns"
}
```

Note: Terraform parses any `source = ` string that does **not** start
with `./` or `../` as a *registry address* (e.g.
`hashicorp/consul/aws`). To reach across the workspace tree, traverse
up to the workspace root with `../..` and back down:

```hcl
module "dns" {
  source = "../../infra/dns"
}
```

## Restrictions

- Only `.tf`, `.tf.json`, `.tftpl`, and `.hcl` files are allowed in
  `srcs`. `.tfvars` and `.tfvars.json` are intentionally rejected there
  — variable values must come through `tf_deploy(vars = {...})`
  or `tf_deploy(var_files = [...])`, not via files committed in
  libraries.
- `var_files` accepts both `.tfvars.json` (JSON) and `.tfvars` (HCL)
  files. Duplicate-key detection covers both formats: the JSON files are
  scanned directly and the HCL files are parsed to catch overlapping keys
  across `vars` and every `var_files` entry.
- Variable keys must be unique across `vars` and all `var_files` entries.
  A dedicated `bazel build` action checks this and fails on any overlap.
- `:<name>.apply` and `:<name>.destroy` are refused when the deploy
  declares no `backend "..." {}` or `cloud {}` block, because the local
  fallback state under `bazel-bin` is machine-local and deleted by
  `bazel clean`. Declare a backend, or set
  `tf_deploy(allow_ephemeral_state = True)` where that loss is harmless.
  `:<name>.plan` is not gated.
- Files from external Bazel modules cannot be included in a deploy: the
  runner cannot give them a sensible workspace-relative path that
  Terraform's local-module addressing can reach.

## TODOs / known gaps

- Build OpenTofu from source via rules_go (currently: download pinned
  binary).
- Cross-compilation: the build-time validate action and the runtime
  runner both consume the exec-platform provider binary, so the runtime
  host must match the build host.
- Windows host support (downloads work; launcher script is bash-only).
- Generated API docs attached to releases (`starlark_doc_extract` plus a
  `docs_url` in `.bcr/source.template.json`).
