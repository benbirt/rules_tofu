package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// runTest implements --command=test for a tf_test target without --state-dir
// or --output-root.
//
// It prepares a run directory in a new directory under TEST_TMPDIR for Bazel
// to remove after the test. When TEST_TMPDIR is unset, it creates the
// directory under TMPDIR and removes it on exit unless it holds the only copy
// of errored_test.tfstate. It initialises with -no-color and -backend=false
// and runs `tofu test -no-color`. When TEST_UNDECLARED_OUTPUTS_DIR is set,
// `tofu test` writes tofu-test.json there. If a run block's destroy fails,
// tofu writes the remaining resources to errored_test.tfstate and still exits
// 0. runTest copies that file to TEST_UNDECLARED_OUTPUTS_DIR when set and
// fails the test.
func runTest(extraArgs []string) error {
	// Bazel removes TEST_TMPDIR. When TEST_TMPDIR is unset, remove the
	// temporary directory on exit unless the error below names
	// errored_test.tfstate inside it.
	tmp := os.Getenv("TEST_TMPDIR")
	scratch, err := os.MkdirTemp(tmp, "rules_tofu-test-")
	if err != nil {
		return err
	}
	remove := tmp == ""
	defer func() {
		if remove {
			os.RemoveAll(scratch)
		}
	}()
	d, err := prepare(scratch)
	if err != nil {
		return err
	}
	dirArgs := testDirArgs()
	varArgs := variableArgs()
	// Test logs are files, so pass -no-color.
	initArgs := append([]string{"-no-color", "-backend=false"}, dirArgs...)
	if err := d.init(os.Stdout, append(initArgs, varArgs...)...); err != nil {
		return err
	}

	args := append([]string{"test", "-no-color"}, dirArgs...)
	outputs := os.Getenv("TEST_UNDECLARED_OUTPUTS_DIR")
	if outputs != "" {
		// Make outputs absolute before passing -json-into to tofu
		// in the run directory.
		if outputs, err = filepath.Abs(outputs); err != nil {
			return err
		}
		args = append(args, "-json-into="+filepath.Join(outputs, "tofu-test.json"))
	}
	args = append(args, varArgs...)
	args = append(args, extraArgs...)
	testErr := runTofu(d.env, d.cwd, args...)

	errored := filepath.Join(d.cwd, "errored_test.tfstate")
	if data, err := os.ReadFile(errored); err == nil {
		kept := errored
		if outputs != "" {
			kept = filepath.Join(outputs, "errored_test.tfstate")
			if err := os.WriteFile(kept, data, 0o644); err != nil {
				return err
			}
		} else {
			remove = false
		}
		return fmt.Errorf("tofu test left resources in state after a run's destroy failed; they need cleaning up by hand from the state file kept at %s", kept)
	}
	if testErr != nil {
		return fmt.Errorf("tofu test: %w", testErr)
	}
	return nil
}

// testDirArgs returns -test-directory=<dir> when --test-dir names a
// subdirectory of the root module, and nil otherwise. "." means the root
// module's own directory, which tofu already reads. An empty --test-dir means
// no test files on validate and OpenTofu's default "tests" directory on test.
func testDirArgs() []string {
	if *testDir != "" && *testDir != "." {
		return []string{"-test-directory=" + *testDir}
	}
	return nil
}
