"""Selects which pinned OpenTofu releases get a per-version integration test.

The module manifest, the `.terraform/terraform.tfstate` backend record and
`tofu show -json -module` output are internal OpenTofu formats. Testing the
floor and the newest pinned release brackets every version users can pick;
each test is a nested Bazel run and a `tofu` download on every CI job (Linux
and macOS), so testing each minor release is not worth the CI time.
"""

def _key(version):
    return tuple([int(part) for part in version.split(".")])

def tested_versions(known_versions):
    """Returns the oldest and the newest version.

    Args:
        known_versions: Pinned versions, such as the keys of
            `KNOWN_VERSIONS`, as `X.Y.Z` strings in any order.

    Returns:
        Selected version strings, oldest first.
    """
    versions = sorted(known_versions, key = _key)
    return versions[:1] + versions[1:][-1:]
