"""Walking the files a repository rule fetched, without following symlinks.

A fetched git repository or archive can hold symlinks of every kind: to a
file or directory of its own, to nothing, to an absolute path on the machine
that built it, or back up to one of its own ancestors. `path.readdir()` lists
them all and `path.is_dir` is true for a symlink to a directory, so a walk
that descends through `is_dir` alone circles through an ancestor link until
it gives up. The walk here never descends into a symlink; the caller decides
what to do with each.
"""

# More directories than any module or provider package holds; a bound on the
# loop, which Starlark needs in place of `while`.
MAX_DIRS = 100000

def walk(root):
    """Every entry below the directory `root`, without following symlinks.

    Args:
        root: the directory to walk, a `path`.

    Returns:
        A list of `struct(path, rel, is_dir, is_symlink)`, one per entry:
        `path` the entry's `path`, `rel` its slash-separated path below
        `root`, `is_dir` whether it is a directory itself (not a symlink to
        one) and `is_symlink` whether it is a symlink, wherever that leads.
        Directories come before their contents; the rest of the order is
        the file system's.
    """
    entries = []
    pending = [(root, str(root.realpath), "")]
    for _ in range(MAX_DIRS):
        if not pending:
            break
        directory, directory_real, rel = pending.pop()
        for child in directory.readdir():
            child_rel = "{}/{}".format(rel, child.basename) if rel else child.basename

            # `exists` and `realpath` follow symlinks: a listed entry that
            # doesn't exist is a symlink to nothing (or to itself), and one
            # whose real path is somewhere else is a symlink to that.
            is_symlink = not child.exists or str(child.realpath) != "{}/{}".format(directory_real, child.basename)
            is_dir = child.is_dir and not is_symlink
            entries.append(struct(path = child, rel = child_rel, is_dir = is_dir, is_symlink = is_symlink))
            if is_dir:
                pending.append((child, "{}/{}".format(directory_real, child.basename), child_rel))
    if pending:
        fail("{} holds more than {} directories".format(root, MAX_DIRS))
    return entries

def within(path, root):
    """Whether the absolute path string `path` is `root` or below it."""
    return path == root or path.startswith(root + "/")
