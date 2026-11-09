"""Build-time duplicate-variable check for `tf_deploy`.

The deploy rule calls `tf_check_var_files(...)` to attach a check stamp
to its outputs, so any overlap between keys in `vars` and `var_files`
(or between two `var_files` entries) fails at `bazel build` time (with
action caching) rather than later at `bazel run` time.
"""

DUPCHECK_BIN = "//tf/private/cmd/dupcheck:dupcheck"

def tf_check_var_files(ctx, vars_keys, var_files):
    """Emit a build-time duplicate-key check across `vars` and `var_files`.

    Args:
      ctx: rule ctx. Must declare `_dupcheck_bin` (cfg = "exec") on the rule.
      vars_keys: list[string] — keys of the deploy's `vars` dict.
      var_files: list[File] — the deploy's var_files inputs.

    Returns:
      File or None: a stamp declared as the action output, or None when
      there's no work to do (no var_files; intra-vars duplication is
      impossible for a Starlark dict).
    """
    if len(var_files) == 0:
        return None

    checker = ctx.attr._dupcheck_bin[DefaultInfo].files_to_run
    stamp = ctx.actions.declare_file(ctx.label.name + ".var_files_check.stamp")

    args = ctx.actions.args()
    args.add("--stamp", stamp)
    args.add_all(vars_keys, before_each = "--vars-key")
    args.add_all(var_files, map_each = _var_file_flags)

    ctx.actions.run(
        executable = checker,
        inputs = var_files,
        outputs = [stamp],
        arguments = [args],
        mnemonic = "RulesTofuVarFilesCheck",
        progress_message = "RulesTofuVarFilesCheck %{label}",
    )
    return stamp

def _var_file_flags(f):
    # `short_path` is the workspace-relative path, which is the most
    # human-recognizable identifier we can hand the user in an error message.
    # `path` is the exec-root-relative path the action will actually read.
    # They travel as two flags, paired by position, because a colon is legal
    # in a target name and so in both, which makes any `label:path` string
    # ambiguous (issue #62).
    return ["--var-file-label=" + f.short_path, "--var-file=" + f.path]
