"""The repository holding rules_tofu's host tools, for module extensions and
repository rules.

Some of rules_tofu's work happens before anything can be built:
`tf_providers.from_lock` reads the lock while its module extension is
evaluated, and `tf_modules.module` describes a fetched module while its
repository rule runs. Neither can run a Bazel target, so this repository rule
compiles the Go tools they need with rules_go's host Go SDK, as Gazelle's
`go_repository_tools` does for its own helpers:

- `lockjson` (//tf/private/cmd/lockjson) parses a `.terraform.lock.hcl`;
- `modscan` (//tf/private/cmd/modscan) lists a module's required providers
  and module calls.

Their Go module dependencies (`hashicorp/hcl` and those it needs) are
downloaded through the Go module proxy (`GOPROXY` is honoured) and checked
against rules_tofu's `go.sum`.

The repository is fetched the first time a tool is needed, and kept until
rules_tofu's sources or the Go SDK change.
"""

load("@go_host_compatible_sdk_label//:defs.bzl", "HOST_COMPATIBLE_SDK")

_TOOLS = ["lockjson", "modscan"]

_SOURCES = [
    Label("//:go.mod"),
    Label("//:go.sum"),
    Label("//tf/private/cmd/internal/flags:flags.go"),
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

    # Everything else (PATH, GOPROXY, proxies, certificates) is inherited
    # from the environment, as for `go` run by hand.
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
        fail("Could not build rules_tofu's host tools with {}:\n{}".format(go, result.stderr))
    repository_ctx.delete(scratch)

    repository_ctx.file(
        "BUILD.bazel",
        "exports_files([{}])\n".format(", ".join(['"bin/{}{}"'.format(tool, exe) for tool in _TOOLS])),
        executable = False,
    )

host_tools = repository_rule(
    implementation = _host_tools_impl,
    environ = ["GOPROXY", "GONOSUMDB", "GOPRIVATE", "GONOPROXY", "GOSUMDB"],
    doc = "Compiles rules_tofu's lockjson and modscan with the host Go SDK.",
)
