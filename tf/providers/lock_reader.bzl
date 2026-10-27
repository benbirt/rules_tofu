"""The repository holding `lockjson`, the `.terraform.lock.hcl` reader.

`tf_providers.from_lock` must read the lock while its module extension is
evaluated, before anything can be built, so the reader can't be a Bazel
target. Instead this repository rule compiles //tf/private/cmd/lockjson with
rules_go's host Go SDK, as Gazelle's `go_repository_tools` does for its own
helpers. Its Go module dependencies (`hashicorp/hcl` and those it needs) are
downloaded through the Go module proxy (`GOPROXY` is honoured) and checked
against rules_tofu's `go.sum`.

The repository is fetched only when a root module's lock is read, and kept
until rules_tofu's sources or the Go SDK change.
"""

load("@go_host_compatible_sdk_label//:defs.bzl", "HOST_COMPATIBLE_SDK")

_SOURCES = [
    Label("//:go.mod"),
    Label("//:go.sum"),
    Label("//tf/private/cmd/lockjson:main.go"),
]

def _lock_reader_impl(repository_ctx):
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
    result = repository_ctx.execute(
        [
            go,
            "build",
            "-trimpath",
            "-buildvcs=false",
            "-o",
            repository_ctx.path("lockjson" + exe),
            "./tf/private/cmd/lockjson",
        ],
        environment = env,
        working_directory = str(module_dir),
        timeout = 900,
    )
    if result.return_code != 0:
        fail("Could not build rules_tofu's .terraform.lock.hcl reader with {}:\n{}".format(go, result.stderr))
    repository_ctx.delete(scratch)

    repository_ctx.file("BUILD.bazel", 'exports_files(["lockjson{}"])\n'.format(exe), executable = False)

lock_reader = repository_rule(
    implementation = _lock_reader_impl,
    environ = ["GOPROXY", "GONOSUMDB", "GOPRIVATE", "GONOPROXY", "GOSUMDB"],
    doc = "Compiles //tf/private/cmd/lockjson with the host Go SDK.",
)
