#!/usr/bin/env bash
# Pin one or more OpenTofu releases in //toolchain:versions.bzl.
#
#   bazel run //tools:update_versions -- 1.12.1 1.12.2            # add to KNOWN_VERSIONS
#   bazel run //tools:update_versions -- --default 1.12.1 1.12.2  # also set DEFAULT_VERSION to the newest
#
# Digests come from each release's SHA256SUMS file, whose cosign signature is
# verified against the OpenTofu release workflow's identity, as documented at
# https://opentofu.org/docs/intro/install/standalone/. The pinned digests are
# what every downstream build trusts, so the point of trust is here, once,
# rather than in each repository fetch. cosign is the sigstore/cosign release
# pinned in MODULE.bazel, reached through runfiles; curl is the host's.

# --- begin runfiles.bash initialization v3 ---
# Copy-pasted from the Bazel Bash runfiles library v3.
set -uo pipefail
set +e
f=bazel_tools/tools/bash/runfiles/runfiles.bash
# shellcheck disable=SC1090
source "${RUNFILES_DIR:-/dev/null}/$f" 2>/dev/null ||
  source "$(grep -sm1 "^$f " "${RUNFILES_MANIFEST_FILE:-/dev/null}" | cut -f2- -d' ')" 2>/dev/null ||
  source "$0.runfiles/$f" 2>/dev/null ||
  source "$(grep -sm1 "^$f " "$0.runfiles_manifest" | cut -f2- -d' ')" 2>/dev/null ||
  source "$(grep -sm1 "^$f " "$0.exe.runfiles_manifest" | cut -f2- -d' ')" 2>/dev/null ||
  {
    echo >&2 "ERROR: cannot find $f"
    exit 1
  }
f=
set -e
# --- end runfiles.bash initialization v3 ---

usage() {
  echo "usage: bazel run //tools:update_versions -- [--default] <version>..." >&2
  exit 2
}

if [ -z "${BUILD_WORKSPACE_DIRECTORY:-}" ]; then
  echo "error: run this with \`bazel run //tools:update_versions -- <version>...\`" >&2
  exit 1
fi

make_default=0
versions=()
for arg in "$@"; do
  case "$arg" in
    --default) make_default=1 ;;
    -h | --help) usage ;;
    -*) usage ;;
    *) versions+=("$arg") ;;
  esac
done
[ "${#versions[@]}" -gt 0 ] || usage

versions_bzl="$BUILD_WORKSPACE_DIRECTORY/toolchain/versions.bzl"
# Reject bad or already-pinned versions before downloading anything.
for version in "${versions[@]}"; do
  if ! [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "error: '$version' is not a release version like 1.12.1" >&2
    exit 2
  fi
  if grep -q "^    \"$version\": {" "$versions_bzl"; then
    echo "error: $version is already in KNOWN_VERSIONS" >&2
    exit 1
  fi
done

# Sorted, so the newest is last; --default takes it.
sorted=()
while IFS= read -r version; do sorted+=("$version"); done < <(printf '%s\n' "${versions[@]}" | sort -u -V)
versions=("${sorted[@]}")
newest="${versions[${#versions[@]} - 1]}"

command -v curl >/dev/null || {
  echo "error: curl is required" >&2
  exit 1
}
# COSIGN is the rlocationpath the sh_binary's `env` sets.
# shellcheck disable=SC2153
cosign="$(rlocation "$COSIGN")"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
cp "$versions_bzl" "$tmp/versions.bzl"

# Verifies the signature on release $1's SHA256SUMS and adds its entry to
# KNOWN_VERSIONS in $tmp/versions.bzl.
pin() {
  local version="$1"
  local base="https://github.com/opentofu/opentofu/releases/download/v$version"
  local sums="tofu_${version}_SHA256SUMS"
  local f
  for f in "$sums" "$sums.sig" "$sums.pem"; do
    curl -fsSL -o "$tmp/$f" "$base/$f" || {
      echo "error: could not download $base/$f" >&2
      exit 1
    }
  done

  # The signing identity is the release workflow on the release branch,
  # which is named after the major.minor version (v1.12 for 1.12.x).
  local major_minor="${version%.*}"
  "$cosign" verify-blob \
    --certificate-identity "https://github.com/opentofu/opentofu/.github/workflows/release.yml@refs/heads/v$major_minor" \
    --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
    --certificate "$tmp/$sums.pem" \
    --signature "$tmp/$sums.sig" \
    "$tmp/$sums" >/dev/null

  # One digest per platform in PLATFORMS, in that order.
  local entry="    \"$version\": {"$'\n'
  local platform digest
  for platform in linux_amd64 linux_arm64 darwin_amd64 darwin_arm64 windows_amd64; do
    digest="$(awk -v zip="tofu_${version}_${platform}.zip" '$2 == zip { print $1 }' "$tmp/$sums")"
    if [ -z "$digest" ]; then
      echo "error: $sums has no entry for $platform" >&2
      exit 1
    fi
    entry+="        \"$platform\": \"$digest\","$'\n'
  done
  entry+="    },"

  # Insert the entry before the first older one, keeping KNOWN_VERSIONS
  # newest first.
  awk -v version="$version" -v entry="$entry" '
        function older(a, b,    x, y, i) {
            split(a, x, ".")
            split(b, y, ".")
            for (i = 1; i <= 3; i++) {
                if (x[i] + 0 != y[i] + 0) return x[i] + 0 < y[i] + 0
            }
            return 0
        }
        /^KNOWN_VERSIONS = \{$/ { known = 1 }
        known && !inserted && (/^}$/ || (/^    "[0-9.]+": \{$/ && older(substr($1, 2, length($1) - 3), version))) {
            print entry
            inserted = 1
        }
        /^}$/ { known = 0 }
        { print }
        END { if (!inserted) exit 1 }
    ' "$tmp/versions.bzl" >"$tmp/versions.bzl.next" || {
    echo "error: KNOWN_VERSIONS not found in $versions_bzl" >&2
    exit 1
  }
  mv "$tmp/versions.bzl.next" "$tmp/versions.bzl"
}

# versions.bzl is written once, after every signature has verified, so a
# failure leaves it unchanged.
for version in "${versions[@]}"; do
  pin "$version"
done

if [ "$make_default" = 1 ]; then
  sed "s/^DEFAULT_VERSION = \".*\"$/DEFAULT_VERSION = \"$newest\"/" "$tmp/versions.bzl" >"$tmp/versions.bzl.default"
  mv "$tmp/versions.bzl.default" "$tmp/versions.bzl"
fi

cp "$tmp/versions.bzl" "$versions_bzl"
echo "Pinned OpenTofu ${versions[*]} in toolchain/versions.bzl (signatures verified)."
if [ "$make_default" = 1 ]; then
  echo "DEFAULT_VERSION is now $newest."
fi
