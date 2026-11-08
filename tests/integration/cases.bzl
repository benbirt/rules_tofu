"""The line format assert_bazel_fails.sh reads its cases in."""

def case_line(case_name, command, target, pattern):
    """One line of the driver's CASES: `name|command|target|fragment`.

    Args:
        case_name: the case's name, which `ONLY_CASES` selects by.
        command: the nested Bazel command, `build` or `run`.
        target: the target inside the sub-workspace.
        pattern: the substring expected in the failing command's stderr.

    Returns:
        The `|`-separated line, after checking no field contains the
        separator or a newline.
    """
    if command not in ("build", "run"):
        fail("case {}: command must be `build` or `run`, not {}".format(case_name, repr(command)))
    for field in (case_name, command, target, pattern):
        if "|" in field or "\n" in field:
            fail("case field {} may contain neither `|` nor a newline".format(repr(field)))
    return "|".join([case_name, command, target, pattern])
