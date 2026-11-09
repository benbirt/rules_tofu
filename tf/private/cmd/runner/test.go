package main

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
)

// runTest is `--command=test`: `tofu test` for a tf_test target, run as a
// Bazel test, so BUILD_WORKSPACE_DIRECTORY and --state-dir play no part.
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
	cwd := filepath.Join(copyRoot, filepath.FromSlash(*packageDir))
	if _, err := os.Stat(cwd); err != nil {
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

	env := append(os.Environ(), "TF_IN_AUTOMATION=1")
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

// copyTree copies the directory src to dst, following symlinks: a work tree
// is a tree of symlinks to the sources, and the copy is what tofu writes in.
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
			return copyTree(p, target)
		}
		return copyFile(p, target)
	})
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}
