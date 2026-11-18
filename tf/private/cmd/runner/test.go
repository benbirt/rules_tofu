package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"

	"github.com/benbirt/rules_tofu/tf/private/cmd/internal/tfconfig"
)

// runTest implements --command=test: `tofu test` for a tf_test target, run as
// a Bazel test, so --state-dir and --output-root play no part.
//
// The work tree is copied into TEST_TMPDIR, or a temporary directory when
// unset, because the runfiles tree is Bazel's and tofu writes .terraform/ and
// the lock beside the configuration. The provider plugin tree is read in
// place when present. Init runs with -backend=false: `tofu test` keeps state
// in memory and ignores backend and cloud blocks, so no backend is contacted.
// When a run's destroy fails, tofu writes the resources it could not remove to
// errored_test.tfstate and still exits 0; runTest fails the test on that file
// and copies it to TEST_UNDECLARED_OUTPUTS_DIR when set.
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
	s, err := stage(scratch)
	if err != nil {
		return err
	}
	if err := s.init(os.Stdout, "-no-color"); err != nil {
		return err
	}
	copyRoot, cwd, env, dirArgs := s.root, s.cwd, s.env, s.dirArgs

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

// staged is a scratch copy of the work tree, set up for `tofu init`.
type staged struct {
	// root is the copy of the work tree and cwd the root module's directory
	// in it.
	root, cwd string
	// plugins is the directory passed to tofu as -plugin-dir.
	plugins string
	// hasConfig is whether the root module's directory holds configuration.
	hasConfig bool
	// env is the environment tofu runs in (see tofuEnv).
	env []string
	// dirArgs is the -test-directory flag, when --test-dir names a
	// directory below the root module's.
	dirArgs []string
}

// stage copies --work-tree into scratch and installs the lock and the module
// manifest in the root module's directory, for --command=test and
// --command=validate.
func stage(scratch string) (*staged, error) {
	s := &staged{root: filepath.Join(scratch, "rules_tofu.work")}
	if err := os.RemoveAll(s.root); err != nil {
		return nil, err
	}
	// A rule with no files at all declares nothing under its work tree, so
	// the directory is absent: an empty module.
	if _, err := os.Stat(*workTree); err == nil {
		if err := copyTree(*workTree, s.root); err != nil {
			return nil, fmt.Errorf("copy work tree: %w", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	// The root module's directory, which the work tree lacks when no file
	// is at or below it: an empty module, as in the run directory of a
	// deploy (see rundir.go).
	s.cwd = filepath.Join(s.root, filepath.FromSlash(*packageDir))
	if err := os.MkdirAll(s.cwd, 0o755); err != nil {
		return nil, fmt.Errorf("work-tree cwd %s: %w", s.cwd, err)
	}

	// With no providers in scope the plugin tree has no files, so Bazel
	// makes no directory for it: give tofu an empty one, and never write
	// into Bazel's tree.
	s.plugins = *pluginDir
	if _, err := os.Stat(s.plugins); err != nil {
		s.plugins = filepath.Join(scratch, "rules_tofu.plugins")
		if err := os.MkdirAll(s.plugins, 0o755); err != nil {
			return nil, fmt.Errorf("create plugin dir %s: %w", s.plugins, err)
		}
	}
	// Not for a root module without configuration: `tofu init` does nothing
	// there, so it installs no provider and leaves the lock as it is, which
	// tofu then holds against the empty plugin cache.
	s.hasConfig = tfconfig.HasConfig(s.cwd)
	if *lockFile != "" && s.hasConfig {
		if err := installLockFile(*lockFile, s.cwd); err != nil {
			return nil, err
		}
	}
	if err := installModulesManifest(*modulesManifest, s.cwd); err != nil {
		return nil, err
	}
	s.env = tofuEnv(s.cwd)
	// The test files' directory, which tofu reads alongside the root module's
	// own. Absent, tofu reads its default, `tests`.
	if *testDir != "" && *testDir != "." {
		s.dirArgs = []string{"-test-directory=" + *testDir}
	}
	return s, nil
}

// init runs `tofu init` offline in the staged root module, with stdout sent
// to stdout and flags added to the command line.
func (s *staged) init(stdout io.Writer, flags ...string) error {
	args := append([]string{"init"}, flags...)
	args = append(args, "-backend=false", "-input=false", "-get=false", "-plugin-dir="+s.plugins)
	args = append(args, s.dirArgs...)
	if err := runTofuTo(stdout, s.env, s.cwd, args...); err != nil {
		if *lockFile != "" {
			log.Print("if the provider versions in required_providers changed, run `bazel run @tf_providers//:pin` to update the lock file.")
		}
		return fmt.Errorf("tofu init: %w", err)
	}
	return nil
}

// copyTree copies the directory src to dst, following symlinks to files: a
// work tree is a tree of symlinks to the sources, and the copy is what tofu
// writes in. A symlink to a directory is an error (see errDirectoryLink).
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
