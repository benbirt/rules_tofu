"""Repository rule compiling `rules_tofu`'s Go host tools for module extensions and repository rules.

Some work runs before Bazel can build any target: `tf_providers.from_lock`
reads the lock during module-extension evaluation, and `tf_modules` parses
module source addresses during extension evaluation and scans fetched modules in
its repository rule. Neither can run a Bazel target, so this repository rule
compiles the Go tools they need with `rules_go`'s host Go SDK, as Gazelle's
`go_repository_tools` does:

- `lockjson` (`//tf/private/cmd/lockjson`) parses `.terraform.lock.hcl`.
- `modscan` (`//tf/private/cmd/modscan`) parses module `source` strings and
  lists a module's `required_providers` and module calls.

Go module dependencies such as `hashicorp/hcl` and OpenTofu's
`registry-address` are downloaded via the Go module proxy, honouring `GOPROXY`,
`GONOSUMDB`, `GOPRIVATE`, `GONOPROXY` and `GOSUMDB`, and verified against
`rules_tofu`'s `go.sum`.

The repository is fetched on first use and re-fetched only when its watched Go
sources, the host Go SDK or those Go proxy environment variables change.
"""

load("@go_host_compatible_sdk_label//:defs.bzl", "HOST_COMPATIBLE_SDK")

_TOOLS = ["lockjson", "modscan"]

_SOURCES = [
    Label("//:go.mod"),
    Label("//:go.sum"),
    Label("//tf/private/cmd/internal/flags:flags.go"),
    Label("//tf/private/cmd/internal/tfconfig:source.go"),
    Label("//tf/private/cmd/internal/tfconfig:tests.go"),
    Label("//tf/private/cmd/internal/tfconfig:tfconfig.go"),
    Label("//tf/private/cmd/lockjson:main.go"),
    Label("//tf/private/cmd/modscan:main.go"),
]

def _host_tools_impl(repository_ctx):
    for src in _SOURCES:
        repository_ctx.watch(repository_ctx.path(src))
    goroot = repository_ctx.path(HOST_COMPATIBLE_SDK).dirname
    exe = ".exe" if "windows" in repository_ctx.os.name.lower() else ""
    go = goroot.get_child("bin").get_child("go" + exe)
    module_dir = repository_ctx.path(Label("//:go.mod")).dirname
    scratch = repository_ctx.path("go")

    # Other variables such as PATH, GOPROXY, HTTP proxies and certificate paths
    # are inherited from the environment, as when running `go` by hand.
    env = {
        "CGO_ENABLED": "0",
        "GOCACHE": str(scratch.get_child("cache")),
        "GOFLAGS": "-mod=readonly -modcacherw",
        "GOMODCACHE": str(scratch.get_child("mod")),
        "GOPATH": str(scratch.get_child("path")),
        "GOROOT": str(goroot),
        "GOTOOLCHAIN": "local",
        "GOWORK": "off",
    }

    # With several packages, `-o` names a directory, which must end in a
    # separator.
    out = str(repository_ctx.path("bin")) + "/"
    result = repository_ctx.execute(
        [go, "build", "-trimpath", "-buildvcs=false", "-o", out] +
        ["./tf/private/cmd/" + tool for tool in _TOOLS],
        environment = env,
        working_directory = str(module_dir),
        timeout = 900,
    )
    if result.return_code != 0:
        fail(host_tools_error(go, result.stderr))
    repository_ctx.delete(scratch)

    repository_ctx.file(
        "BUILD.bazel",
        "exports_files([{}])\n".format(", ".join(['"bin/{}{}"'.format(tool, exe) for tool in _TOOLS])),
        executable = False,
    )

# What `go build` prints when the SDK is older than go.mod's `go` directive;
# Go checks that before compiling anything.
_SDK_TOO_OLD = "requires go >="

def host_tools_error(go, stderr):
    """The message for a `go build` of the host tools that failed.

    Pure, so //tf/providers/tests can check it. Go's own diagnostics are kept.
    When the SDK is older than rules_tofu's `go.mod` requires, a hint says
    where that SDK comes from and how to choose a newer one, neither of which
    the Go line says (issue #101).

    Args:
        go: The `go` binary that was run.
        stderr: What it wrote to stderr.

    Returns:
        The string to `fail` with.
    """
    hint = ""
    if _SDK_TOO_OLD in stderr:
        hint = (
            "\nrules_tofu's lockjson and modscan are compiled with rules_go's host Go SDK: the first " +
            "host-compatible SDK in the module graph, the root module's first. Choose one at least as new " +
            "as the `go` directive in rules_tofu's go.mod, with `go_sdk.download(version = \"...\")` in the " +
            "root MODULE.bazel or a newer `go` directive in the file given to `go_sdk.from_file`."
        )
    return "Could not build rules_tofu's host tools with {}:\n{}{}".format(go, stderr.strip(), hint)

host_tools = repository_rule(
    implementation = _host_tools_impl,
    environ = ["GOPROXY", "GONOSUMDB", "GOPRIVATE", "GONOPROXY", "GOSUMDB"],
    doc = "Compiles rules_tofu's lockjson and modscan with the host Go SDK.",
)
