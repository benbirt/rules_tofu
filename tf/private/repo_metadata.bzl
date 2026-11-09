"""Declaring a repository's contents reproducible, so Bazel keeps them."""

def reproducible(repository_ctx):
    """What a repository rule returns when its inputs determine its contents.

    Bazel 9 then keeps the fetched directory in its repo contents cache, shared
    by every workspace of the user: another output base (a nested integration
    test, a second checkout) gets the repository without fetching it. The
    condition is that two fetches with the same attributes, watched files and
    declared environment produce the same files, which holds while every
    download the rule makes is checked against a digest those inputs carry.
    Bazel 8.5 has no `repo_metadata`; there this changes nothing.

    Args:
        repository_ctx: the rule's context.

    Returns:
        The value for the rule's implementation to return.
    """
    if hasattr(repository_ctx, "repo_metadata"):
        return repository_ctx.repo_metadata(reproducible = True)
    return None
