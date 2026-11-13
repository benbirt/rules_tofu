package main

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
)

// runTest is `--command=test`: `tofu test` for a tf_test target, run as a
// Bazel test, so --state-dir and --output-root play no part.
//
// The work tree is copied into $TEST_TMPDIR first: the runfiles tree is
// Bazel's, and tofu writes `.terraform/` and the lock beside the
// configuration. The provider plugin tree is read in place. Init runs with
// -backend=false: `tofu test` keeps state in memory and ignores backend and
// cloud blocks, so a backend is never contacted. When a run's destroy fails,
// tofu writes the resources it couldn't remove to `errored_test.tfstate` and
// still exits 0, so that file fails the test and is kept with its outputs.
func runTest(extraArgs []string) error {
	scratch := os.Getenv("TEST_TMPDIR")
	if scratch == "" {
		dir, err := os.MkdirTemp("", "rules_tofu-test-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		scratch = dir
	}
	copyRoot := filepath.Join(scratch, "rules_tofu.work")
	if err := os.RemoveAll(copyRoot); err != nil {
		return err
	}
	if err := copyTree(*workTree, copyRoot); err != nil {
		return fmt.Errorf("copy work tree: %w", err)
	}
	// The root module's directory, which the work tree lacks when no file
	// is at or below it: an empty module, as in the run directory of a
	// deploy (rundir.go).
	cwd := filepath.Join(copyRoot, filepath.FromSlash(*packageDir))
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		return fmt.Errorf("work-tree cwd %s: %w", cwd, err)
	}

	// With no providers in scope the plugin tree has no files, so the
	// runfiles tree lacks the directory: give tofu an empty one, and never
	// write into the runfiles tree.
	plugins := *pluginDir
	if _, err := os.Stat(plugins); err != nil {
		plugins = filepath.Join(scratch, "rules_tofu.plugins")
		if err := os.MkdirAll(plugins, 0o755); err != nil {
			return fmt.Errorf("create plugin dir %s: %w", plugins, err)
		}
	}
	if *lockFile != "" {
		if err := installLockFile(*lockFile, cwd); err != nil {
			return err
		}
	}
	if err := installModulesManifest(*modulesManifest, cwd); err != nil {
		return err
	}

	env := tofuEnv(cwd)
	// The test files' directory, which tofu reads alongside the root module's
	// own. Absent, tofu reads its default, `tests`.
	var dirArgs []string
	if *testDir != "" {
		dirArgs = []string{"-test-directory=" + *testDir}
	}
	initArgs := append([]string{"init", "-no-color", "-backend=false", "-input=false", "-get=false", "-plugin-dir=" + plugins}, dirArgs...)
	if err := runTofu(env, cwd, initArgs...); err != nil {
		if *lockFile != "" {
			log.Print("if the provider versions in required_providers changed, run `bazel run @tf_providers//:pin` to update the lock file.")
		}
		return fmt.Errorf("tofu init: %w", err)
	}

	// Test logs are files, so no colour.
	args := append([]string{"test", "-no-color"}, dirArgs...)
	outputs := os.Getenv("TEST_UNDECLARED_OUTPUTS_DIR")
	if outputs != "" {
		args = append(args, "-json-into="+filepath.Join(outputs, "tofu-test.json"))
	}
	// Var files are given relative to the work tree, which has moved.
	for _, f := range *varFiles {
		if !filepath.IsAbs(f) {
			f = filepath.Join(copyRoot, filepath.FromSlash(f))
		}
		args = append(args, "-var-file="+f)
	}
	args = append(args, extraArgs...)
	testErr := runTofu(env, cwd, args...)

	errored := filepath.Join(cwd, "errored_test.tfstate")
	if _, err := os.Stat(errored); err == nil {
		kept := errored
		if outputs != "" {
			kept = filepath.Join(outputs, "errored_test.tfstate")
			if err := copyFile(errored, kept); err != nil {
				return err
			}
		}
		return fmt.Errorf("tofu test left resources in state after a run's destroy failed; they need cleaning up by hand from the state file kept at %s", kept)
	}
	if testErr != nil {
		return fmt.Errorf("tofu test: %w", testErr)
	}
	return nil
}

// copyTree copies the directory src to dst, following symlinks to files: a
// work tree is a tree of symlinks to the sources, and the copy is what tofu
// writes in. A symlink to a directory is an error (errDirectoryLink).
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := os.Stat(p)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return errDirectoryLink(p)
		}
		return copyFile(p, target)
	})
}

// errDirectoryLink is the error for a symlink to a directory in a work tree.
// The rules never make one: every entry is a declared file, which Bazel
// refuses to link to a directory, and a fetched module's are removed. One
// that got there some other way is refused rather than walked: WalkDir
// doesn't descend into it, and walking it by hand never ends on a link that
// leads back to one of its own ancestors (issue #89).
func errDirectoryLink(p string) error {
	return fmt.Errorf("%s is a symlink to a directory; a work tree holds files and real directories only", p)
}

// copyFile copies the file src to dst, following a symlink. The copy is
// writable whatever src is, and executable if src is: a script that a
// `local-exec` provisioner or an `external` data source runs must still run
// from the copy. Not src's own mode: Bazel's outputs are read-only, and tofu
// and the next copy write here.
func copyFile(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	mode := fs.FileMode(0o644)
	if info.Mode()&0o111 != 0 {
		mode = 0o755
	}
	return os.WriteFile(dst, data, mode)
}
