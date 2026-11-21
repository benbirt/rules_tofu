"""Selects which pinned OpenTofu releases get a per-version integration test.

Internal OpenTofu formats that rules_tofu depends on change between minor
releases rather than patches, so testing the floor and the newest patch of
each minor covers each format boundary without running a nested Bazel test
for every pinned patch.
"""

def _key(version):
    return tuple([int(part) for part in version.split(".")])

def tested_versions(known_versions):
    """Returns the oldest version and the newest patch of each minor release.

    Args:
        known_versions: Pinned versions, such as the keys of
            `KNOWN_VERSIONS`, as `X.Y.Z` strings in any order.

    Returns:
        Selected version strings, oldest first.
    """
    versions = sorted(known_versions, key = _key)
    newest = {}
    for version in versions:
        newest[_key(version)[:2]] = version
    selected = versions[:1]
    for version in newest.values():
        if version not in selected:
            selected.append(version)
    return selected
