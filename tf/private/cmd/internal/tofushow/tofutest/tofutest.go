// Package tofutest resolves the `tofu` binary for Go tests that read modules
// with tofushow.
package tofutest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/benbirt/rules_tofu/tf/private/cmd/internal/tofushow"
)

// Reader returns a tofushow.Reader for path, the value of a test's -tofu flag.
// When path is empty (as under plain `go test` without -tofu), Reader calls
// t.Skip. Under `bazel test`, relative rlocation paths from
// `-tofu=$(rlocationpath //tofu)` are resolved against RUNFILES_DIR or
// TEST_SRCDIR; under `go test ./tf/private/cmd/...`, pass an absolute
// -tofu=<path> because `go test` runs each package in its own directory.
//
// Reader also makes relative RUNFILES_DIR and TEST_SRCDIR paths absolute and
// clears BUILD_WORKING_DIRECTORY for the test so the //tofu launcher finds its
// binary and stays in ReadFiles's working directory.
func Reader(t *testing.T, path string) tofushow.Reader {
	t.Helper()
	if path == "" {
		t.Skip("tofu binary path not set; pass -tofu=<path> or run via bazel test")
	}
	// ReadFiles runs the //tofu launcher in a scratch working directory, where
	// relative RUNFILES_DIR or TEST_SRCDIR paths would break.
	resolved := filepath.IsAbs(path)
	for _, name := range []string{"RUNFILES_DIR", "TEST_SRCDIR"} {
		dir := os.Getenv(name)
		if dir == "" {
			continue
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			t.Fatal(err)
		}
		if abs != dir {
			t.Setenv(name, abs)
		}
		if !resolved {
			path = filepath.Join(abs, path)
			resolved = true
		}
	}
	// Under `bazel run` the //tofu launcher would change to this directory.
	if os.Getenv("BUILD_WORKING_DIRECTORY") != "" {
		t.Setenv("BUILD_WORKING_DIRECTORY", "")
	}
	// Resolve any relative path now, before the test changes working directory.
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return tofushow.Reader{Tofu: abs}
}
