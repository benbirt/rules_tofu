// Command runner wraps OpenTofu's plan, apply, destroy and test commands for
// generated tf_runner and tf_test launchers. Each tf_deploy target generates
// .plan, .apply and .destroy launchers via tf_runner. Launchers are ordinary
// executables: they work under `bazel run` or `bazel test` and when run
// directly, and runner reads nothing about the checkout such as
// BUILD_WORKSPACE_DIRECTORY.
//
// Flags:
//   - --tofu, --work-tree, --plugin-dir, --modules-manifest and --command:
//     required for every invocation. --command is "plan", "apply", "destroy" or
//     "test".
//   - --state-dir and --output-root: required for plan, apply and destroy.
//   - --package-dir: root module directory relative to --work-tree, or "" for
//     the workspace root package.
//   - --lock-file: .terraform.lock.hcl from tf_providers.from_lock.
//   - --var-file: .tfvars or .tfvars.json file passed via -var-file,
//     repeatable.
//   - --test-dir: for --command=test, test directory relative to --package-dir,
//     passed as -test-directory; "" uses OpenTofu's default "tests".
//   - --allow-ephemeral-state: permit apply and destroy when state would live
//     under --output-root.
//
// Trailing arguments after flags are forwarded verbatim to tofu.
//
// For plan, apply and destroy, runner performs these steps in order:
//   - Mirror --work-tree as symlinks into <state-dir>/work, removing the
//     previous run's lock, .terraform/providers and
//     .terraform/terraform.tfstate (see rundir.go). The deploy's .plan, .apply
//     and .destroy share that directory so OpenTofu resolves relative paths
//     consistently across launchers.
//   - Copy any --lock-file to .terraform.lock.hcl and --modules-manifest to
//     .terraform/modules/modules.json in <state-dir>/work/<package-dir>, then
//     run `tofu init -input=false -get=false -plugin-dir=<plugin-dir>` there
//     with the -var-file flags, TF_DATA_DIR set to its .terraform and
//     TF_PLUGIN_CACHE_DIR unset.
//   - For apply and destroy without --allow-ephemeral-state, read the backend
//     recorded in .terraform/terraform.tfstate (see backend.go) and refuse if
//     the record cannot be read or state is ephemeral: no backend or cloud
//     block, or a local backend unless path and workspace_dir are both
//     absolute paths outside --output-root.
//   - Run `tofu plan -input=false -out=<state-dir>/tfplan` (printing
//     "rules_tofu: plan saved to <path>" to stdout), `tofu apply -input=false`
//     or `tofu destroy -input=false`, with the -var-file flags and any
//     trailing arguments.
//
// Runner passes no -state, -state-out or -backend-config flags and never
// rewrites a backend block: OpenTofu owns where state lives. Without a backend
// or cloud block, state is <state-dir>/work/<package-dir>/terraform.tfstate.
//
// For test, runner copies --work-tree into TEST_TMPDIR or a temporary
// directory, initialises with -backend=false and runs `tofu test` there (see
// test.go). If a failed destroy leaves errored_test.tfstate behind, runner
// fails the test even though `tofu test` exits 0. When
// TEST_UNDECLARED_OUTPUTS_DIR is set, runner writes tofu-test.json there and
// copies any errored_test.tfstate into it.
//
// When a tofu subprocess exits non-zero, runner exits with that code unless
// errored_test.tfstate supersedes it with exit code 1. Unrecognised or
// malformed flags exit 2, and other errors exit 1. Configuration validation
// via TofuValidate on tf_library, tf_deploy and tf_test and duplicate-variable
// checks via dupcheck happen at `bazel build` time, not here.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/benbirt/rules_tofu/tf/private/cmd/internal/flags"
)

var (
	workTree        = flag.String("work-tree", "", "path to the materialised work tree root")
	packageDir      = flag.String("package-dir", "", "work-tree-relative directory of the root module; empty for the workspace root package")
	pluginDir       = flag.String("plugin-dir", "", "absolute path to the vendored provider plugin tree, passed to tofu init via -plugin-dir")
	varFiles        = flags.StringList("var-file", "path to a .tfvars or .tfvars.json file passed to tofu via -var-file (repeatable)")
	modulesManifest = flag.String("modules-manifest", "", "the .terraform/modules/modules.json recording every module call, copied into place before tofu init -get=false")
	lockFile        = flag.String("lock-file", "", "optional .terraform.lock.hcl pinning the providers (tf_providers.from_lock), copied next to the configuration before tofu init")
	testDir         = flag.String("test-dir", "", "for --command=test, the test files' directory relative to the package dir, passed to tofu as -test-directory; empty for tofu's default")

	stateDir   = flag.String("state-dir", "", "absolute path to the per-deploy state directory; tofu runs in work/<package-dir> beneath it")
	outputRoot = flag.String("output-root", "", "absolute path to Bazel's output tree (bazel-out); a local backend's state inside it is ephemeral")

	allowEphemeralState = flag.Bool("allow-ephemeral-state", false,
		"permit apply/destroy when state is ephemeral: no backend, or a local backend whose path and workspace_dir are not both absolute paths outside --output-root; "+
			"set by tf_deploy(allow_ephemeral_state = True)")

	tofu    = flag.String("tofu", "", "path to the tofu binary")
	command = flag.String("command", "", `"plan", "apply", "destroy" or "test"`)
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("rules_tofu: ")
	flag.Parse()

	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.ExitCode())
		}
		os.Exit(1)
	}
}

func run() error {
	extraArgs := flag.Args()

	// --package-dir is deliberately absent: an empty value is valid and means
	// the deploy lives in the workspace root package, so the work tree root
	// itself holds the root module.
	for name, value := range map[string]string{
		"--tofu":             *tofu,
		"--work-tree":        *workTree,
		"--command":          *command,
		"--plugin-dir":       *pluginDir,
		"--modules-manifest": *modulesManifest,
	} {
		if value == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	switch *command {
	case "plan", "apply", "destroy":
	case "test":
		return runTest(extraArgs)
	default:
		return fmt.Errorf(`--command must be "plan", "apply", "destroy" or "test", got %q`, *command)
	}
	if *stateDir == "" {
		return errors.New("--state-dir is required")
	}
	if *outputRoot == "" {
		return errors.New("--output-root is required")
	}

	// tofu runs in another directory, so every path handed to it, and every
	// link into the work tree, must be absolute. The launcher passes absolute
	// paths; this covers a hand-run runner.
	for _, p := range append([]*string{workTree, pluginDir, stateDir, outputRoot, tofu}, varFilePtrs()...) {
		abs, err := filepath.Abs(*p)
		if err != nil {
			return err
		}
		*p = abs
	}

	srcDir := filepath.Join(*workTree, filepath.FromSlash(*packageDir))
	if _, err := os.Stat(srcDir); err != nil {
		return fmt.Errorf("work tree package directory %s: %w", srcDir, err)
	}
	runRoot := filepath.Join(*stateDir, "work")
	cwd := filepath.Join(runRoot, filepath.FromSlash(*packageDir))

	planFile := filepath.Join(*stateDir, "tfplan")

	if err := os.MkdirAll(*stateDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", *stateDir, err)
	}
	if err := syncRunDir(*workTree, runRoot, cwd); err != nil {
		return err
	}

	env := tofuEnv(cwd)

	// Ensure the vendored plugin tree exists even when zero providers are in
	// scope: the deploy declares no symlinks under <name>.plugins/ in that
	// case, so the runfiles tree lacks the directory. -plugin-dir overrides
	// default plugin search paths and prevents runtime registry lookups.
	if err := os.MkdirAll(*pluginDir, 0o755); err != nil {
		return fmt.Errorf("create plugin dir %s: %w", *pluginDir, err)
	}
	if *lockFile != "" {
		if err := installLockFile(*lockFile, cwd); err != nil {
			return err
		}
	}
	if err := installModulesManifest(*modulesManifest, cwd); err != nil {
		return err
	}
	varFileArgs := make([]string, len(*varFiles))
	for i, f := range *varFiles {
		varFileArgs[i] = "-var-file=" + f
	}

	// Removing the previous backend record above makes tofu init read any
	// backend or cloud block afresh without `bazel clean`. -reconfigure is not
	// passed because tofu rejects it when a cloud block is present. -var-file
	// flags are passed because backend and cloud blocks may reference
	// variables.
	initArgs := append([]string{"init", "-input=false", "-get=false", "-plugin-dir=" + *pluginDir}, varFileArgs...)
	if err := runTofu(env, cwd, initArgs...); err != nil {
		if *lockFile != "" {
			log.Print("if the provider versions in required_providers changed, run `bazel run @tf_providers//:pin` to update the lock file.")
		}
		if localState := filepath.Join(cwd, defaultStateFile); hasContent(localState) {
			log.Printf("if tofu needs approval for state migration from %s, in its directory run `tofu init -migrate-state` for a backend block or `tofu init` for a cloud block, or delete that file.", localState)
		}
		return fmt.Errorf("tofu init: %w", err)
	}

	// Check the backend recorded by tofu init before mutating infrastructure.
	// Running tofu init first touches no infrastructure, and plan is left
	// ungated.
	if (*command == "apply" || *command == "destroy") && !*allowEphemeralState {
		if err := checkStateDurable(*command, cwd, *outputRoot); err != nil {
			return err
		}
	}

	switch *command {
	case "plan":
		args := append([]string{"plan", "-input=false", "-out=" + planFile}, varFileArgs...)
		args = append(args, extraArgs...)
		if err := runTofu(env, cwd, args...); err != nil {
			return fmt.Errorf("tofu plan: %w", err)
		}
		fmt.Printf("rules_tofu: plan saved to %s\n", planFile)
	case "apply":
		applyArgs := append([]string{"apply", "-input=false"}, varFileArgs...)
		applyArgs = append(applyArgs, extraArgs...)
		if err := runTofu(env, cwd, applyArgs...); err != nil {
			return fmt.Errorf("tofu apply: %w", err)
		}
	case "destroy":
		destroyArgs := append([]string{"destroy", "-input=false"}, varFileArgs...)
		destroyArgs = append(destroyArgs, extraArgs...)
		if err := runTofu(env, cwd, destroyArgs...); err != nil {
			return fmt.Errorf("tofu destroy: %w", err)
		}
	}
	return nil
}

// hasContent reports whether path is a file that is not empty.
func hasContent(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}

// varFilePtrs returns a pointer to each --var-file value.
func varFilePtrs() []*string {
	ptrs := make([]*string, len(*varFiles))
	for i := range *varFiles {
		ptrs[i] = &(*varFiles)[i]
	}
	return ptrs
}

// installLockFile copies the provider lock to cwd/.terraform.lock.hcl, so
// init checks the configuration's constraints and the vendored packages
// against it. Whatever is there already is replaced, never written through.
func installLockFile(src, cwd string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read lock file: %w", err)
	}
	dst := filepath.Join(cwd, ".terraform.lock.hcl")
	if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("replace %s: %w", dst, err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", dst, err)
	}
	return nil
}

// installModulesManifest copies the module manifest to
// cwd/.terraform/modules/modules.json, where `tofu init -get=false` reads
// which module calls are installed where. As with the lock, a manifest an
// earlier run left there is replaced, never written through.
func installModulesManifest(src, cwd string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read module manifest: %w", err)
	}
	dir := filepath.Join(cwd, ".terraform", "modules")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	dst := filepath.Join(dir, "modules.json")
	if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("replace %s: %w", dst, err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", dst, err)
	}
	return nil
}

// tofuEnv returns os.Environ() adjusted for running tofu in working directory
// cwd:
//   - TF_DATA_DIR is fixed to cwd/.terraform, where installModulesManifest
//     wrote modules.json. Inheriting a user's TF_DATA_DIR would make
//     `tofu init -get=false` miss the manifest at run time while build-time
//     validation, which runs in a clean environment, succeeded.
//   - TF_PLUGIN_CACHE_DIR is stripped so tofu neither consults nor writes a
//     shared cache beside the vendored plugin tree.
//   - TF_IN_AUTOMATION=1 suppresses CLI usage hints aimed at unwrapped runs.
//   - TF_INPUT is left unset: -input=false suppresses variable prompts per
//     command while keeping the apply and destroy approval prompt reachable.
func tofuEnv(cwd string) []string {
	var env []string
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "TF_DATA_DIR=") || strings.HasPrefix(e, "TF_PLUGIN_CACHE_DIR=") || strings.HasPrefix(e, "TF_IN_AUTOMATION=") {
			continue
		}
		env = append(env, e)
	}
	return append(env, "TF_IN_AUTOMATION=1", "TF_DATA_DIR="+filepath.Join(cwd, ".terraform"))
}

func runTofu(env []string, cwd string, args ...string) error {
	cmd := exec.Command(*tofu, args...)
	cmd.Dir = cwd
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
