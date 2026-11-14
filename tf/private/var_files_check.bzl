"""Build-time duplicate-variable check for `tf_deploy`.

`tf_deploy` calls `tf_check_var_files` and attaches its stamp to the target's
outputs, so any duplicate key across `vars` and `var_files` or between two
`var_files` entries fails in a cached `bazel build` action rather than at
`bazel run` time.
"""

DUPCHECK_BIN = "//tf/private/cmd/dupcheck:dupcheck"

def tf_check_var_files(ctx, vars_keys, var_files):
    """Emit a build-time duplicate-key check across `vars` and `var_files`.

    Args:
      ctx: rule ctx. Must declare `_dupcheck_bin` (cfg = "exec") on the rule.
      vars_keys: list[string], keys of the deploy's `vars` dict.
      var_files: list[File], the deploy's `var_files` inputs.

    Returns:
      File or None: a stamp declared as the action output, or None when
      `var_files` is empty, since duplicate keys within a Starlark `vars` dict
      are impossible.
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
    # `short_path` is workspace-relative for main-repository files and
    # ../<repo>/... for external ones, giving a readable path to report in
    # errors. `path` is the exec-root-relative path the action reads. They
    # travel as two flags paired by position because colons are legal in Bazel
    # target names and therefore in both paths, making any `label:path` string
    # ambiguous (issue #62).
    return ["--var-file-label=" + f.short_path, "--var-file=" + f.path]
