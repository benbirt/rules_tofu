package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// runDir holds the paths and environment prepared for a tofu invocation.
type runDir struct {
	// root mirrors the work tree at <base>/work. cwd is the run directory
	// inside root where tofu runs.
	root, cwd string
	// plugins is the provider plugin directory passed via -plugin-dir.
	plugins string
	// hasConfig reports whether cwd contains any configuration files.
	hasConfig bool
	// env is the environment passed to tofu.
	env []string
}

// prepare mirrors --work-tree into base/work as real directories and symlinks
// to files. It then prepares the run directory base/work/<package-dir> with
// the module manifest, any applicable lock file, the plugin directory and
// tofu's environment.
//
// tofu cannot run in --work-tree itself. Under bazel-bin, <name>.work is a
// build-action input that must not gain files written by tofu. In runfiles,
// each launcher has its own tree that Bazel prunes. Running there would make
// .plan, .apply and .destroy resolve relative paths to different directories.
// Running in base/work keeps .terraform/, the lock and any local state in a
// directory the runner owns.
//
// Plan, apply and destroy pass --state-dir as base and reuse it across runs.
// Test and validate pass a fresh temporary directory. Outside .terraform/,
// every symlink in base/work was placed there by the runner. Updating a
// reused directory therefore needs no record of earlier runs. clearRunDir
// removes old symlinks and three stale tofu files. linkTree then links the
// current work tree. Other regular files belong to tofu or the user and are
// kept. A regular file at a work-tree path fails the run so it cannot shadow
// configuration.
func prepare(base string) (*runDir, error) {
	// Make base absolute because TMPDIR or TEST_TMPDIR may be relative.
	// Paths derived from base are passed to tofu in the run directory.
	base, err := filepath.Abs(base)
	if err != nil {
		return nil, err
	}
	d := &runDir{root: filepath.Join(base, "work")}
	d.cwd = filepath.Join(d.root, filepath.FromSlash(*packageDir))
	if err := clearRunDir(d.root, d.cwd); err != nil {
		return nil, err
	}
	// A rule with no files declares nothing under <name>.work. Treat a
	// missing --work-tree as an empty module.
	if _, err := os.Stat(*workTree); err == nil {
		if err := linkTree(*workTree, d.root); err != nil {
			return nil, fmt.Errorf("populate the run directory %s: %w", d.root, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	// Create the run directory when --work-tree has no file at or below
	// --package-dir (issue #119).
	if err := os.MkdirAll(d.cwd, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", d.cwd, err)
	}

	// With no providers in scope, Bazel declares nothing under --plugin-dir
	// and creates no directory for it. Give tofu an empty directory in base
	// rather than writing into Bazel's tree.
	d.plugins = *pluginDir
	if _, err := os.Stat(d.plugins); err != nil {
		d.plugins = filepath.Join(base, "plugins")
		if err := os.MkdirAll(d.plugins, 0o755); err != nil {
			return nil, fmt.Errorf("create plugin dir %s: %w", d.plugins, err)
		}
	}
	// Skip the lock when the run directory has no configuration. `tofu init`
	// installs no providers there and leaves the lock unpruned. `tofu validate`
	// and `tofu test` would then fail against the empty .terraform/providers
	// directory.
	d.hasConfig = hasConfig(d.cwd)
	if *lockFile != "" && d.hasConfig {
		if err := installLockFile(*lockFile, d.cwd); err != nil {
			return nil, err
		}
	}
	if err := installModulesManifest(*modulesManifest, d.cwd); err != nil {
		return nil, err
	}
	d.env = tofuEnv(d.cwd)
	return d, nil
}

// init runs `tofu init` offline in d.cwd with args appended after the offline
// flags. Standard output goes to stdout.
func (d *runDir) init(stdout io.Writer, args ...string) error {
	args = append([]string{"init", "-input=false", "-get=false", "-plugin-dir=" + d.plugins}, args...)
	if err := runTofuTo(stdout, d.env, d.cwd, args...); err != nil {
		if *lockFile != "" {
			log.Print("if the provider versions in required_providers changed, run `bazel run @tf_providers//:pin` to update the lock file.")
		}
		return fmt.Errorf("tofu init: %w", err)
	}
	return nil
}

// clearRunDir creates root if needed and removes stale entries from an
// earlier run. It deletes every symlink in root outside cwd/.terraform and
// removes three files that `tofu init` wrote in cwd.
func clearRunDir(root, cwd string) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", root, err)
	}
	dataDir := filepath.Join(cwd, ".terraform")
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
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
		return fmt.Errorf("clear the run directory %s: %w", root, err)
	}

	// Remove three files from the previous run before `tofu init`:
	//   - .terraform.lock.hcl records that build's provider versions. Any
	//     managed lock is copied in afterwards.
	//   - .terraform/providers links into that run's plugin tree.
	//   - .terraform/terraform.tfstate caches backend settings (see
	//     backend.go). Removing it makes `tofu init` read the current
	//     configuration afresh and leaves no stale record when a backend
	//     block is deleted.
	if err := os.Remove(filepath.Join(cwd, ".terraform.lock.hcl")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove the previous run's lock file: %w", err)
	}
	if err := os.RemoveAll(filepath.Join(dataDir, "providers")); err != nil {
		return fmt.Errorf("remove the previous run's provider links: %w", err)
	}
	if err := os.Remove(backendRecordPath(cwd)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove the previous run's backend record: %w", err)
	}
	return nil
}

// linkTree creates a symlink under dst at the same relative path for every
// file under src. Because src is absolute, each symlink target is absolute.
// Directories are created rather than linked so files written beside the
// configuration never land in src. A symlink to a directory is rejected.
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
			// Remove an empty directory left when an earlier run had a
			// directory at a path that is now a file.
			if !existing.IsDir() || os.Remove(target) != nil {
				return fmt.Errorf("%s is in the way of the configuration's file of that name: move it away or delete it", target)
			}
		}
		return os.Symlink(p, target)
	})
}

// errDirectoryLink reports a symlink to a directory in a work tree (issue
// #89). The rules never create one because Bazel rejects directory targets on
// declared file symlinks and tf_modules strips them when unpacking. WalkDir
// does not descend into a directory symlink. Walking one by hand would loop
// on a link to an ancestor.
func errDirectoryLink(p string) error {
	return fmt.Errorf("%s is a symlink to a directory; a work tree holds files and real directories only", p)
}

// hasConfig reports whether dir contains a non-dot .tf, .tf.json, .tofu or
// .tofu.json entry. It duplicates tfconfig.HasConfig so the runner binary does
// not link the HCL parser; keep the two aligned.
func hasConfig(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		for _, ext := range []string{".tf", ".tf.json", ".tofu", ".tofu.json"} {
			if strings.HasSuffix(e.Name(), ext) {
				return true
			}
		}
	}
	return false
}
