# Sourced by the //tests/integration drivers once _TMPDIR is set: where the
# nested Bazel keeps its caches, and `bazel`, the function that runs it.
#
# Bazel sees TEST_TMPDIR and puts the nested Bazel's output user root under
# it, so every test would extract an install base and fetch each repository
# from nothing (the Go SDK alone is ten seconds). Give it the outer Bazel's
# user root instead, Bazel's own default for the platform (HOME is inherited
# by bazel_integration_test): the install base and the repository and repo
# contents caches are then shared with the outer Bazel and across tests,
# while the output base below stays per test. The nested `bazel` binary is a
# bazelisk wrapper that likewise caches its Bazel download under $PWD unless
# told otherwise; use its per-user cache, which setup-bazel saves on CI.
_user="${USER:-$(id -un)}"
if [[ "$(uname -s)" == Darwin ]]; then
  NESTED_USER_ROOT="/var/tmp/_bazel_${_user}"
  : "${BAZELISK_HOME:=${HOME}/Library/Caches/bazelisk}"
else
  NESTED_USER_ROOT="${XDG_CACHE_HOME:-${HOME}/.cache}/bazel/_bazel_${_user}"
  : "${BAZELISK_HOME:=${XDG_CACHE_HOME:-${HOME}/.cache}/bazelisk}"
fi
export BAZELISK_HOME

# Its own output base, in the test's scratch directory. Were it to share the
# outer Bazel's (as it did when setup-bazel put --output_base in ~/.bazelrc:
# https://github.com/bazel-contrib/setup-bazel/issues/108), it would try to
# kill the outer server, the process running this test.
NESTED_OUTPUT_BASE="${_TMPDIR}/nested_output_base"

# gazelle's go_repository downloads each Go module into a cache private to
# the output base, so every nested Bazel fetched hcl, OpenTofu's
# registry-address library and the rest again, several seconds apiece. With
# this set it uses the user's own module and build caches (`go env
# GOMODCACHE` and `GOCACHE`), as `go` run by hand does, shared across tests.
export GO_REPOSITORY_USE_HOST_CACHE=1

# --output_user_root and --output_base are startup options (before the
# command), so they override anything set in ~/.bazelrc. Drivers that hand
# a `bazel` to another program (pin's $BAZEL) write a wrapper with the same
# two options.
bazel() {
  "${BIT_BAZEL_BINARY}" --output_user_root="${NESTED_USER_ROOT}" --output_base="${NESTED_OUTPUT_BASE}" "$@"
}

# The nested Bazel is the version of the Bazel running this test
# (bazel_binaries.download in MODULE.bazel takes it from @running_bazel), so
# that a run on another Bazel than the pinned one, such as CI's on the oldest
# supported, tests the nested builds on it too. Check it is: they once ran on
# the pinned version whatever ran the tests (issue #88). Every
# bazel_integration_test sets RUNNING_BAZEL_VERSION in its env.
_running="${RUNNING_BAZEL_VERSION:?set it in the env of the bazel_integration_test, as the others in tests/integration/BUILD.bazel do}"
_nested="$("${BIT_BAZEL_BINARY}" --version)"
if [[ "${_nested}" != "bazel ${_running}" ]]; then
  echo "FAIL: the nested Bazel is '${_nested}', but Bazel ${_running} is running this test" >&2
  exit 1
fi
