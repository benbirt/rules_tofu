#!/usr/bin/env bash
# Pin a new OpenTofu release in //toolchain:versions.bzl.
#
#   toolchain/update_versions.sh 1.12.1            # add to KNOWN_VERSIONS
#   toolchain/update_versions.sh --default 1.12.1  # ...and make it DEFAULT_VERSION
#
# The digests come from the release's SHA256SUMS file, whose cosign signature
# is verified first against the OpenTofu release workflow's identity, as
# https://opentofu.org/docs/intro/install/standalone/ documents. The pinned
# digests are what every downstream build trusts, so the point of trust is
# here, once, rather than in each repository fetch. Needs curl and cosign
# (https://docs.sigstore.dev/cosign/system_config/installation/).
set -euo pipefail

usage() {
    echo "usage: $0 [--default] <version>" >&2
    exit 2
}

make_default=0
version=""
for arg in "$@"; do
    case "$arg" in
        --default) make_default=1 ;;
        -h | --help) usage ;;
        -*) usage ;;
        *)
            [ -z "$version" ] || usage
            version="$arg"
            ;;
    esac
done
[ -n "$version" ] || usage
if ! [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "error: '$version' is not a release version like 1.12.1" >&2
    exit 2
fi

versions_bzl="$(cd "$(dirname "$0")" && pwd)/versions.bzl"
if grep -q "^    \"$version\": {" "$versions_bzl"; then
    echo "error: $version is already in KNOWN_VERSIONS" >&2
    exit 1
fi

for tool in curl cosign; do
    command -v "$tool" >/dev/null || { echo "error: $tool is required" >&2; exit 1; }
done

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

base="https://github.com/opentofu/opentofu/releases/download/v$version"
sums="tofu_${version}_SHA256SUMS"
for f in "$sums" "$sums.sig" "$sums.pem"; do
    curl -fsSL -o "$tmp/$f" "$base/$f" || { echo "error: could not download $base/$f" >&2; exit 1; }
done

# The signing identity is the release workflow on the release branch, which
# is named after the major.minor version (v1.12 for 1.12.x).
major_minor="${version%.*}"
cosign verify-blob \
    --certificate-identity "https://github.com/opentofu/opentofu/.github/workflows/release.yml@refs/heads/v$major_minor" \
    --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
    --certificate "$tmp/$sums.pem" \
    --signature "$tmp/$sums.sig" \
    "$tmp/$sums" >/dev/null

# One digest per platform in PLATFORMS, in that order.
entry="    \"$version\": {"$'\n'
for platform in linux_amd64 linux_arm64 darwin_amd64 darwin_arm64 windows_amd64; do
    digest="$(awk -v zip="tofu_${version}_${platform}.zip" '$2 == zip { print $1 }' "$tmp/$sums")"
    if [ -z "$digest" ]; then
        echo "error: $sums has no entry for $platform" >&2
        exit 1
    fi
    entry+="        \"$platform\": \"$digest\","$'\n'
done
entry+="    },"

# Insert the entry right after the opening of KNOWN_VERSIONS.
awk -v entry="$entry" '
    { print }
    /^KNOWN_VERSIONS = \{$/ { print entry; inserted = 1 }
    END { if (!inserted) exit 1 }
' "$versions_bzl" >"$tmp/versions.bzl" || { echo "error: KNOWN_VERSIONS not found in $versions_bzl" >&2; exit 1; }

if [ "$make_default" = 1 ]; then
    sed "s/^DEFAULT_VERSION = \".*\"$/DEFAULT_VERSION = \"$version\"/" "$tmp/versions.bzl" >"$tmp/versions.bzl.default"
    mv "$tmp/versions.bzl.default" "$tmp/versions.bzl"
fi

cp "$tmp/versions.bzl" "$versions_bzl"
echo "Pinned OpenTofu $version in $versions_bzl (signature verified)."
if [ "$make_default" = 1 ]; then
    echo "DEFAULT_VERSION is now $version."
fi
