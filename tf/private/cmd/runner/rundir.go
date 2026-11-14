package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// syncRunDir makes runRoot, the deploy's run directory, a mirror of workTree,
// the launcher's work tree: every file of the work tree appears at the same
// relative path as a symlink to it. cwd is the root module's directory in
// runRoot, where tofu runs.
//
// tofu can't run in the work tree itself. A launcher's runfiles copy of it is
// per-target, so .plan, .apply and .destroy would each resolve a relative
// path to a different file, in a tree Bazel prunes; and the declared one under
// bazel-bin is an input to build actions, which must not find tofu's files in
// it. The run directory is the runner's own, one per deploy, and holds
// whatever tofu writes beside the configuration: .terraform/, the lock and
// the state of a `backend "local"` with relative paths.
//
// The links are what the runner put there, and the only links in the run
// directory outside tofu's .terraform/, so bringing it up to date needs no
// record of earlier runs: remove every link, then link the current work tree.
// Regular files are tofu's or the user's and are never touched, bar the two
// stale ones noted below. One in the way of a link is an error: it would
// shadow a file of the configuration.
func syncRunDir(workTree, runRoot, cwd string) error {
	if err := os.MkdirAll(runRoot, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", runRoot, err)
	}
	dataDir := filepath.Join(cwd, ".terraform")
	err := filepath.WalkDir(runRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == dataDir && d.IsDir() {
			return filepath.SkipDir
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return os.Remove(p)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("clear the run directory %s: %w", runRoot, err)
	}

	// Left by an earlier run, and wrong for this one: the lock tofu wrote
	// records the provider versions of that build, and .terraform/providers
	// links into that launcher's plugin tree, which init relinks.
	// installLockFile writes any managed lock after this.
	if err := os.Remove(filepath.Join(cwd, ".terraform.lock.hcl")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove the previous run's lock file: %w", err)
	}
	if err := os.RemoveAll(filepath.Join(dataDir, "providers")); err != nil {
		return fmt.Errorf("remove the previous run's provider links: %w", err)
	}

	if err := linkTree(workTree, runRoot); err != nil {
		return fmt.Errorf("populate the run directory %s: %w", runRoot, err)
	}
	// The root module's directory holds at least the generated variables
	// file, so it exists by now; make sure, for tofu's sake.
	return os.MkdirAll(cwd, 0o755)
}

// linkTree creates, for every file under src, a symlink to it at the same
// relative path under dst. src is absolute, so the links are too. Directories
// are created rather than linked, so that nothing tofu writes beside the
// configuration can land in src. A symlink to a directory is an error (see
// errDirectoryLink).
func linkTree(src, dst string) error {
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
			return nil
		}
		info, err := os.Stat(p)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return errDirectoryLink(p)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("%s is in the way of the configuration: %w", filepath.Dir(target), err)
		}
		if existing, err := os.Lstat(target); err == nil {
			// An empty directory holds nothing to lose: the work tree had a
			// directory here on an earlier run and has a file now.
			if !existing.IsDir() || os.Remove(target) != nil {
				return fmt.Errorf("%s is in the way of the configuration's file of that name: move it away or delete it", target)
			}
		}
		return os.Symlink(p, target)
	})
}
