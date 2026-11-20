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
func Reader(t *testing.T, path string) tofushow.Reader {
	t.Helper()
	if path == "" {
		t.Skip("tofu binary path not set; pass -tofu=<path> or run via bazel test")
	}
	if !filepath.IsAbs(path) {
		for _, name := range []string{"RUNFILES_DIR", "TEST_SRCDIR"} {
			if dir := os.Getenv(name); dir != "" {
				path = filepath.Join(dir, path)
				break
			}
		}
	}
	// Reader.ReadFiles runs `tofu` in a temporary working directory.
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return tofushow.Reader{Tofu: abs}
}
