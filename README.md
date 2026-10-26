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
or the release notes, which carry a copy-pasteable snippet):

```python
bazel_dep(name = "rules_tofu", version = "X.Y.Z")

tofu = use_extension("@rules_tofu//toolchain:extensions.bzl", "tofu")
# tofu.version(version = "1.8.5")  # optional; defaults to a pinned version
use_repo(tofu, "tofu_toolchains")
```

One OpenTofu toolchain is registered for the whole build, so all
`tofu.version` tags in the module graph resolve to a single version: a tag
in your root module wins outright (declare at most one), a version asked
for by dependencies alone is honoured if they all agree, and dependencies
asking for different versions is an error telling you to pin one yourself.
With no tags anywhere, the version pinned in
[`toolchain/versions.bzl`](toolchain/versions.bzl) is used.

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

### Providers

Providers are vendored by Bazel and passed to the rules that use them, so
`tofu init` never downloads anything. There are two ways to say which
versions to vendor, and they can be mixed:

- **A lock file** (`tf_providers.from_lock`): versions live in `.tf` files
  and a standard `.terraform.lock.hcl`, as in a plain OpenTofu project, so
  Renovate and Dependabot can keep them up to date.
- **Tags in `MODULE.bazel`** (`tf_providers.provider`): each provider and
  version is declared in Bazel, optionally with per-platform digests or a
  mirror. This suits a module that others depend on, and providers not on the
  OpenTofu registry.

A provider can't be both tagged in the root module and locked. A
dependency's tag for a provider the root locks is overridden by the lock, as
it would be by a root tag.

#### From a lock file

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

BUILD files name providers, never versions: `@tf_providers//<namespace>/<name>`
is an alias onto the provider's repository, so only the providers a build
uses are downloaded. `@tf_providers` lists providers from tags too.

The workspace has one lock, and each provider one version across the build.
Pin the versions next to the lock, and give reusable modules at most a `>=`
floor (OpenTofu's own advice for root and child modules):

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

Then write the lock, and rewrite it whenever a pin changes:

```sh
bazel run @tf_providers//:pin
```

`pin` runs `tofu providers lock` with the toolchain's tofu, for the
`required_providers` of the `.tf` files beside the lock and for
`linux_amd64`, `linux_arm64`, `darwin_amd64`, `darwin_arm64` and
`windows_amd64`. Pass `-platform=<os>_<arch>` arguments to choose others. The
lock doesn't need to exist beforehand.

For each provider in the lock, the extension asks the OpenTofu registry for
each platform's zip and accepts it only if its SHA256 is one of the lock's
`zh:` hashes. Builds copy the lock next to the configuration, so `tofu init`
also checks the unpacked provider against its `h1:` hashes. It fails if the
lock no longer satisfies the configuration's version constraints, and the
error then suggests running `pin`. A floor above the locked version is
caught this way. To catch a `versions.tf` edited by hand without re-pinning,
build it as a library:

```python
tf_library(
    name = "versions",
    srcs = ["versions.tf"],
    providers = ["@tf_providers//hashicorp/aws"],
)
```

A lock entry the extension can't use, such as one locked by `terraform`
against `registry.terraform.io`, fails when that provider is fetched and
names the problem. `@tf_providers//:pin` keeps working, so you can fix the
lock with it. `examples/lock/` is a full end-to-end use.

Keeping the lock up to date:

- **Renovate** works with its default `terraform` manager. It bumps
  `versions.tf` and rewrites the lock in the same PR, and leaves `>=`
  floors alone.
- **Dependabot** needs `package-ecosystem: "opentofu"`, pointed at the lock's
  directory. Its `terraform` ecosystem runs HashiCorp's `terraform` against
  `registry.terraform.io` and can't update an OpenTofu lock.

#### Tags in `MODULE.bazel`

```python
tf_providers = use_extension(
    "@rules_tofu//tf/providers:extensions.bzl",
    "tf_providers",
)
tf_providers.provider(
    source = "hashicorp/aws",
    version = "5.70.0",
)
use_repo(tf_providers, "tf_providers_hashicorp_aws")
```

```python
tf_library(
    name = "s3_bucket_lib",
    srcs = ["main.tf"],
    providers = ["@tf_providers_hashicorp_aws//:provider"],
)
```

The repository name is derived from `source`: `hashicorp/aws` →
`@tf_providers_hashicorp_aws`. When several modules in the dependency graph
tag the same provider, the root module's version wins if it sets one;
otherwise the highest requested version is selected.

With no `sha256`, the extension asks the registry which of `linux_amd64`,
`linux_arm64`, `darwin_amd64`, `darwin_arm64` and `windows_amd64` it
publishes for that version, and their SHA256s, and records them as facts in
`MODULE.bazel.lock`. Later builds, and fresh clones with that lockfile,
reuse the recorded digests without asking the registry again. So commit
`MODULE.bazel.lock` to keep builds reproducible, and use
`--lockfile_mode=error` in CI to keep it honest.

To pin instead, give each platform you'll run on its `shasum` from the
registry's download endpoint:

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

If a pin disagrees with the registry, the fetch fails before downloading the
zip, naming the registry's value. Either way, the registry must be reachable
whenever the provider's repository is fetched. To avoid that, or to use a
mirror, give the tag a `url_template` and pin its `sha256` (with no registry
in the way, there's nothing else to take the digests from):

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
end-to-end uses.

#### How providers are vendored

Either way, providers come from the
[OpenTofu registry](https://search.opentofu.org/) (unless a tag sets
`url_template`), which says where each platform's zip is and what its
SHA256 is: the same binaries a plain `tofu init` fetches. For `hashicorp/`
providers those are OpenTofu's builds (from
`github.com/opentofu/terraform-provider-*`), not HashiCorp's, so their
checksums differ from releases.hashicorp.com's. A digest, pinned on a tag or
recorded in `MODULE.bazel.lock`, gates every download. Sources are
case-insensitive and normalized to lower case, as OpenTofu does, so
repository names are lower case too (`PostHog/posthog` →
`@tf_providers_posthog_posthog`).

A `tf_deploy` may declare its own `providers = [...]`; the union of a
deploy's direct providers and the providers contributed by its library
`deps` is the set that gets vendored. The exec-platform package (the binary
and the rest of its zip) is symlinked alongside the deploy's work tree at
Terraform's canonical `<host>/<namespace>/<name>/<version>/<os>_<arch>/`
layout, and `tofu init` is run with `-plugin-dir=<that tree>`.

The effect is that `tofu init` — both the build-time `validate` action and
the runtime invocation behind `:foo.plan` — never contacts a registry: no
network is required to resolve providers once the zips are in Bazel's
repository cache. The providers' own backends are obviously not offline;
`:foo.plan` and `:foo.apply` still talk to AWS / GCP / etc. at runtime as
usual. A `.terraform.lock.hcl` listed in `srcs`/`data` is rejected: the
only lock rules_tofu uses is the `from_lock` one.

See `examples/hello/` for a minimal example that exercises `tf_library` and
`tf_deploy` with no providers at all.

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

Then:

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
`tf_deploy`) materializes the work tree AND runs
`tofu init -backend=false && tofu validate` as a build action, so
configuration syntax/reference errors fail the build (with action
caching). `bazel test //...` runs the `:foo.fmt_check` test targets;
validation does not live in a separate `:foo.validate` target — building
the rule itself is the validation contract.

At runtime the launcher invokes a small Go binary that cd's into the
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
