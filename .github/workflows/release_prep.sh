#!/usr/bin/env bash
#
# Builds the release source and docs archives and prints release notes to
# stdout. Invoked by the bazel-contrib/.github release_ruleset reusable
# workflow with the tag as $1.
#
# Archive exclusions live here rather than in .gitattributes so ordinary
# `git archive` runs are unaffected. Drop examples/, tests/, tools/,
# toolchain/tests/, tf/providers/tests/ and tf/modules/tests/: consumers do not
# need them and they reference dev-only repos unavailable to downstream
# modules. Drop docs/ because it feeds the docs archive below. Git pathspecs
# are prefix-anchored, so a tests package nested under a shipped directory
# needs its own exclusion. Keep e2e/ and .bcr/, which BCR reads from the
# extracted archive (e2e/smoke and the .bcr/ templates).
set -o errexit -o nounset -o pipefail

# Tag, e.g. v0.1.0. Strip the leading 'v' to match strip_prefix
# ("{REPO}-{VERSION}") in .bcr/source.template.json and mirror the layout
# GitHub uses for source archives.
TAG=$1
PREFIX="rules_tofu-${TAG:1}"
ARCHIVE="rules_tofu-$TAG.tar.gz"

git archive --format=tar --prefix="${PREFIX}/" "${TAG}" \
    -- . ':(exclude)examples' ':(exclude)tests' ':(exclude)tools' ':(exclude)toolchain/tests' ':(exclude)tf/providers/tests' ':(exclude)tf/modules/tests' ':(exclude)docs' | gzip > "$ARCHIVE"

# API docs for registry.bazel.build, which renders every .binaryproto in the
# archive named by `docs_url` in .bcr/source.template.json (see
# https://github.com/bazelbuild/bazel-central-registry/issues/5593). The
# workflow has checked out the tag, so this documents the release. Bazel's
# output goes to stderr, keeping stdout for the release notes.
DOCS_ARCHIVE="${ARCHIVE%.tar.gz}.docs.tar.gz"
bazel build //docs:all
bazel_bin="$(bazel info bazel-bin)"
(cd "$bazel_bin" && find docs -name '*.binaryproto' -print0 | sort -z |
    tar --create --gzip --null --files-from=- --file=-) > "$DOCS_ARCHIVE"

cat << EOF
## Using Bzlmod

Add to your \`MODULE.bazel\`:

\`\`\`starlark
bazel_dep(name = "rules_tofu", version = "${TAG:1}")

tofu = use_extension("@rules_tofu//toolchain:extensions.bzl", "tofu")
use_repo(tofu, "tofu_toolchains")
\`\`\`
EOF
