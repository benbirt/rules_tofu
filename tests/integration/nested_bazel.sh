# Sourced by the //tests/integration drivers once _TMPDIR is set: where the
# nested Bazel keeps its caches, and `bazel`, the function that runs it.
#
# Bazel sets TEST_TMPDIR, so by default the nested Bazel puts its output user
# root there and extracts an install base and every repository from scratch
# (the Go SDK alone takes ten seconds). Point --output_user_root at the
# platform's default user root instead, using HOME inherited through
# bazel_integration_test. The install base, repository cache and repo contents
# cache are then shared with the outer Bazel and across tests, while
# --output_base below stays per-test.
#
# The nested bazel binary is a bazelisk wrapper that caches its Bazel
# download under $PWD unless told otherwise. Point BAZELISK_HOME at its
# per-user cache, which setup-bazel saves on CI.
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

# Check that the nested Bazel matches the outer Bazel version (@running_bazel
# in MODULE.bazel), so CI runs on another Bazel such as 8.5.0 test the nested
# builds on that version too. Nested builds once ran on the pinned version
# regardless of what ran the tests (issue #88); every bazel_integration_test
# sets RUNNING_BAZEL_VERSION in its env.
_running="${RUNNING_BAZEL_VERSION:?set it in the env of the bazel_integration_test, as the others in tests/integration/BUILD.bazel do}"
_nested="$("${BIT_BAZEL_BINARY}" --version)"
if [[ "${_nested}" != "bazel ${_running}" ]]; then
  echo "FAIL: the nested Bazel is '${_nested}', but Bazel ${_running} is running this test" >&2
  exit 1
fi
