package main_test

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var runnerBinPath = flag.String("runner", "", "path to the runner binary under test")

// testManifest stands in for the module manifest tf_deploy generates.
const testManifest = `{"Modules":[{"Key":"","Source":"","Dir":"."}]}`

func runnerBin(t *testing.T) string {
	t.Helper()
	if *runnerBinPath == "" {
		t.Skip("runner binary path not set; pass -runner=<path> or run via bazel test")
	}
	if filepath.IsAbs(*runnerBinPath) {
		return *runnerBinPath
	}
	// Under bazel test, the flag is an rlocation path; resolve it against the
	// runfiles directory so exec.Command receives an absolute path.
	if dir := os.Getenv("RUNFILES_DIR"); dir != "" {
		return filepath.Join(dir, *runnerBinPath)
	}
	if dir := os.Getenv("TEST_SRCDIR"); dir != "" {
		return filepath.Join(dir, *runnerBinPath)
	}
	return *runnerBinPath
}

// invocation records a single call to the fake tofu binary.
type invocation struct {
	cwd  string
	args []string
	// env holds the variables the fake tofu logs (TF_DATA_DIR,
	// TF_PLUGIN_CACHE_DIR), with "<unset>" for one not in its environment.
	env map[string]string
}

func (inv invocation) hasArg(arg string) bool {
	for _, a := range inv.args {
		if a == arg {
			return true
		}
	}
	return false
}

func (inv invocation) hasArgWithPrefix(prefix string) bool {
	for _, a := range inv.args {
		if strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}

// parseInvocations reads the structured log written by the fake tofu script.
// Each invocation is a block of lines:
//
//	cwd=<working directory>
//	arg=<arg1>
//	arg=<arg2>
//	env=<NAME>=<value or <unset>>
//	---
func parseInvocations(data []byte) []invocation {
	var invs []invocation
	var cur *invocation
	for _, line := range strings.Split(string(data), "\n") {
		if line == "---" {
			if cur != nil {
				invs = append(invs, *cur)
				cur = nil
			}
			continue
		}
		if cur == nil {
			cur = &invocation{env: map[string]string{}}
		}
		switch {
		case strings.HasPrefix(line, "cwd="):
			cur.cwd = strings.TrimPrefix(line, "cwd=")
		case strings.HasPrefix(line, "arg="):
			cur.args = append(cur.args, strings.TrimPrefix(line, "arg="))
		case strings.HasPrefix(line, "env="):
			name, value, _ := strings.Cut(strings.TrimPrefix(line, "env="), "=")
			cur.env[name] = value
		}
	}
	return invs
}

// fakeTofuScript returns a stand-in tofu script that logs its working
// directory, arguments and controlled environment variables, runs extra as
// shell commands, writes $TOFU_BACKEND_RECORD (when set on init) to
// $TF_DATA_DIR/terraform.tfstate, and exits 0.
func fakeTofuScript(extra string) string {
	return "#!/bin/sh\n" +
		"{ printf 'cwd=%s\\n' \"$(pwd)\"; for a in \"$@\"; do printf 'arg=%s\\n' \"$a\"; done; " +
		"printf 'env=TF_DATA_DIR=%s\\n' \"${TF_DATA_DIR-<unset>}\"; " +
		"printf 'env=TF_PLUGIN_CACHE_DIR=%s\\n' \"${TF_PLUGIN_CACHE_DIR-<unset>}\"; " +
		"printf -- '---\\n'; } >> \"$TOFU_LOG\"\n" +
		extra +
		"if [ \"$1\" = init ] && [ -n \"${TOFU_BACKEND_RECORD-}\" ]; then\n" +
		"  mkdir -p \"$TF_DATA_DIR\" && printf '%s' \"$TOFU_BACKEND_RECORD\" > \"$TF_DATA_DIR/terraform.tfstate\"\n" +
		"fi\n" +
		"exit 0\n"
}

// setup creates a temporary work tree holding variables.tf in the package
// directory and a fake tofu script that logs each invocation to a temp file.
// It returns:
//   - a pre-configured runner Cmd to which callers may append flags
//   - addVarFile: writes a .tfvars.json file and returns its path
//   - invocations: reads and returns all recorded tofu invocations
func setup(t *testing.T) (
	cmd *exec.Cmd,
	addVarFile func(map[string]any) string,
	invocations func() []invocation,
) {
	t.Helper()
	return setupPkg(t, "mypkg")
}

// setupPkg is like setup but places the deploy at the given workspace-relative
// package directory. An empty pkgDir models a deploy in the workspace root
// package, where the work tree root itself holds the root module.
//
// The runner is given a state directory `<tmp>/state`, so tofu runs in
// `<tmp>/state/work/<pkgDir>` (see runDir), and an output root `<tmp>/out`.
func setupPkg(t *testing.T, pkgDir string) (
	cmd *exec.Cmd,
	addVarFile func(map[string]any) string,
	invocations func() []invocation,
) {
	t.Helper()
	bin := runnerBin(t)
	dir := t.TempDir()

	logPath := filepath.Join(dir, "tofu-invocations.log")
	fakeTofuSh := filepath.Join(dir, "tofu.sh")
	if err := os.WriteFile(fakeTofuSh, []byte(fakeTofuScript("")), 0o755); err != nil {
		t.Fatal(err)
	}

	workTree := filepath.Join(dir, "work")
	if err := os.MkdirAll(filepath.Join(workTree, pkgDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workTree, pkgDir, "variables.tf"), []byte("# variables.tf\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var varFileCounter int
	addVarFile = func(contents map[string]any) string {
		varFileCounter++
		path := filepath.Join(dir, fmt.Sprintf("varfile%d.tfvars.json", varFileCounter))
		writeJSON(t, path, contents)
		return path
	}

	invocations = func() []invocation {
		t.Helper()
		data, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatalf("read tofu invocation log: %v", err)
		}
		return parseInvocations(data)
	}

	pluginDir := filepath.Join(dir, "plugins")
	modulesManifest := filepath.Join(dir, "modules.json")
	if err := os.WriteFile(modulesManifest, []byte(testManifest), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd = exec.Command(bin,
		"--tofu="+fakeTofuSh,
		"--work-tree="+workTree,
		"--package-dir="+pkgDir,
		"--plugin-dir="+pluginDir,
		"--modules-manifest="+modulesManifest,
		"--state-dir="+filepath.Join(dir, "state"),
		"--output-root="+filepath.Join(dir, "out"),
		"--command=plan",
	)
	// The runner must not depend on the checkout: BUILD_WORKSPACE_DIRECTORY is
	// taken out of its environment, as when a built launcher is run directly.
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "BUILD_WORKSPACE_DIRECTORY=") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Env = append(cmd.Env, "TOFU_LOG="+logPath)
	return cmd, addVarFile, invocations
}

func TestRunner_NoVarFiles(t *testing.T) {
	cmd, _, invocations := setup(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	invs := invocations()
	if len(invs) != 2 {
		t.Fatalf("expected 2 tofu invocations (init, plan), got %d: %+v", len(invs), invs)
	}
	init := invs[0]
	if !init.hasArg("init") {
		t.Errorf("first invocation should be 'init', got args: %v", init.args)
	}
	if !init.hasArg("-input=false") {
		t.Errorf("init invocation missing -input=false, got args: %v", init.args)
	}
	if !init.hasArgWithPrefix("-plugin-dir=") {
		t.Errorf("init invocation missing -plugin-dir=, got args: %v", init.args)
	}
	if !init.hasArg("-get=false") {
		t.Errorf("init invocation missing -get=false, got args: %v", init.args)
	}
	installed, err := os.ReadFile(filepath.Join(init.cwd, ".terraform", "modules", "modules.json"))
	if err != nil || string(installed) != testManifest {
		t.Errorf("module manifest not installed before init: %q, %v", installed, err)
	}
	plan := invs[1]
	if !plan.hasArg("plan") {
		t.Errorf("second invocation should be 'plan', got args: %v", plan.args)
	}
	if !plan.hasArg("-input=false") {
		t.Errorf("plan invocation missing -input=false, got args: %v", plan.args)
	}
	if plan.hasArgWithPrefix("-var-file=") {
		t.Errorf("plan invocation should have no -var-file, got args: %v", plan.args)
	}
}

func TestRunner_VarFileNoOverlap(t *testing.T) {
	cmd, addVarFile, invocations := setup(t)
	vf := addVarFile(map[string]any{"env": "prod"})
	cmd.Args = append(cmd.Args, "--var-file="+vf)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	invs := invocations()
	if len(invs) != 2 {
		t.Fatalf("expected 2 tofu invocations (init, plan), got %d: %+v", len(invs), invs)
	}
	plan := invs[1]
	if !plan.hasArg("-var-file=" + vf) {
		t.Errorf("plan invocation missing -var-file=%s, got args: %v", vf, plan.args)
	}
	if !plan.hasArg("-input=false") {
		t.Errorf("plan invocation missing -input=false, got args: %v", plan.args)
	}
}

// TestRunner_WithDataFile checks that arbitrary files from the data attribute
// sit in the work tree for OpenTofu's file() function without interfering with
// runner execution.
func TestRunner_WithDataFile(t *testing.T) {
	cmd, _, invocations := setup(t)

	// Simulate a data file materialised by the data attribute at its short_path
	// under the work tree.
	workTree := ""
	for _, arg := range cmd.Args {
		if strings.HasPrefix(arg, "--work-tree=") {
			workTree = strings.TrimPrefix(arg, "--work-tree=")
		}
	}
	if workTree == "" {
		t.Fatal("could not find --work-tree flag in runner cmd args")
	}
	dataPath := filepath.Join(workTree, "mypkg", "config.json")
	if err := os.WriteFile(dataPath, []byte(`{"key":"value"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed with data file present: %v\n%s", err, out)
	}
	invs := invocations()
	if len(invs) != 2 {
		t.Fatalf("expected 2 tofu invocations (init, plan), got %d: %+v", len(invs), invs)
	}
	if !invs[0].hasArg("init") {
		t.Errorf("first invocation should be 'init', got args: %v", invs[0].args)
	}
	if !invs[1].hasArg("plan") {
		t.Errorf("second invocation should be 'plan', got args: %v", invs[1].args)
	}
}

func TestRunner_ExtraArgsPassedToPlan(t *testing.T) {
	cmd, _, invocations := setup(t)
	cmd.Args = append(cmd.Args, "--", "--target=aws_instance.foo")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	invs := invocations()
	if len(invs) != 2 {
		t.Fatalf("expected 2 tofu invocations (init, plan), got %d: %+v", len(invs), invs)
	}
	plan := invs[1]
	if !plan.hasArg("--target=aws_instance.foo") {
		t.Errorf("plan invocation missing extra arg, got args: %v", plan.args)
	}
}

func TestRunner_ExtraArgsPassedToApply(t *testing.T) {
	cmd, _, invocations := setup(t)
	cmd.Args = append(cmd.Args,
		"--command=apply",
		// No backend block here, so apply needs the ephemeral-state opt-in.
		"--allow-ephemeral-state",
		"--", "--replace=aws_instance.foo",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	invs := invocations()
	if len(invs) != 2 {
		t.Fatalf("expected 2 tofu invocations (init, apply), got %d: %+v", len(invs), invs)
	}
	apply := invs[1]
	if !apply.hasArg("apply") {
		t.Errorf("second invocation should be 'apply', got args: %v", apply.args)
	}
	if !apply.hasArg("--replace=aws_instance.foo") {
		t.Errorf("apply invocation missing extra arg, got args: %v", apply.args)
	}
}

func TestRunner_ExtraArgsPassedToDestroy(t *testing.T) {
	cmd, _, invocations := setup(t)
	cmd.Args = append(cmd.Args,
		"--command=destroy",
		// No backend block here, so destroy needs the ephemeral-state opt-in.
		"--allow-ephemeral-state",
		"--", "--target=aws_instance.foo",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	invs := invocations()
	if len(invs) != 2 {
		t.Fatalf("expected 2 tofu invocations (init, destroy), got %d: %+v", len(invs), invs)
	}
	destroy := invs[1]
	if !destroy.hasArg("destroy") {
		t.Errorf("second invocation should be 'destroy', got args: %v", destroy.args)
	}
	if !destroy.hasArg("--target=aws_instance.foo") {
		t.Errorf("destroy invocation missing extra arg, got args: %v", destroy.args)
	}
}

// backendRecord marshals a .terraform/terraform.tfstate backend record in
// OpenTofu 1.12's format.
func backendRecord(t *testing.T, typ string, config map[string]any) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"version": 3,
		"backend": map[string]any{"type": typ, "config": config, "hash": 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// setBackendRecord configures the fake tofu's init to write record to
// .terraform/terraform.tfstate. When unset, init writes no record, matching a
// deploy with no backend or cloud block.
func setBackendRecord(cmd *exec.Cmd, record string) {
	cmd.Env = append(cmd.Env, "TOFU_BACKEND_RECORD="+record)
}

// recordPath returns the backend record path in the run directory for mypkg.
func recordPath(t *testing.T, cmd *exec.Cmd) string {
	t.Helper()
	return filepath.Join(runDir(t, cmd), "mypkg", ".terraform", "terraform.tfstate")
}

// Neither init nor plan, apply or destroy receives -state, -state-out or
// -backend-config flags, whether or not a backend is recorded.
func TestRunner_NoStateFlags(t *testing.T) {
	for _, withBackend := range []bool{false, true} {
		for _, command := range []string{"plan", "apply", "destroy"} {
			t.Run(fmt.Sprintf("%s/backend=%v", command, withBackend), func(t *testing.T) {
				cmd, _, invocations := setup(t)
				if withBackend {
					setBackendRecord(cmd, backendRecord(t, "s3", map[string]any{"bucket": "b"}))
				}
				if out, err := withCommand(cmd, command, "--allow-ephemeral-state").CombinedOutput(); err != nil {
					t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
				}
				invs := invocations()
				if len(invs) != 2 || !invs[0].hasArg("init") || !invs[1].hasArg(command) {
					t.Fatalf("expected init then %s, got %+v", command, invs)
				}
				for _, inv := range invs {
					if inv.hasArgWithPrefix("-state") || inv.hasArgWithPrefix("-backend-config") {
						t.Errorf("%s args = %v, want no -state, -state-out or -backend-config", inv.args[0], inv.args)
					}
				}
			})
		}
	}
}

// Both tofu init and the subcommand receive every --var as -var=<name>=<value>
// followed by every --var-file as -var-file=<path>, regardless of how --var
// and --var-file are interleaved on the runner command line. OpenTofu
// evaluates variables in backend and cloud blocks during init. Each --var
// value is passed as a single argument preserving spaces, quotes, `$` and
// extra `=` characters. Trailing arguments go only to the subcommand after
// those flags so they override earlier values.
func TestRunner_VarsThenVarFiles(t *testing.T) {
	for _, command := range []string{"plan", "apply", "destroy"} {
		t.Run(command, func(t *testing.T) {
			cmd, addVarFile, invocations := setup(t)
			first := addVarFile(map[string]any{"env": "prod"})
			second := addVarFile(map[string]any{"state_root": "/var/lib/tofu"})
			vars := []string{`name=it's "a b" = $HOME`, `tags={"k"="v"}`, "empty="}
			flags := []string{"--allow-ephemeral-state", "--var-file=" + first, "--var=" + vars[0], "--var-file=" + second, "--var=" + vars[1], "--var=" + vars[2], "--", "-var=name=override"}
			if out, err := withCommand(cmd, command, flags...).CombinedOutput(); err != nil {
				t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
			}
			want := []string{"-var=" + vars[0], "-var=" + vars[1], "-var=" + vars[2], "-var-file=" + first, "-var-file=" + second}
			invs := invocations()
			if len(invs) != 2 {
				t.Fatalf("got %d tofu invocations, want init then %s: %+v", len(invs), command, invs)
			}
			for i, inv := range invs {
				var got []string
				for _, a := range inv.args {
					if strings.HasPrefix(a, "-var") {
						got = append(got, a)
					}
				}
				wantInv := want
				if i == 1 {
					wantInv = append(append([]string{}, want...), "-var=name=override")
				}
				if !reflect.DeepEqual(got, wantInv) {
					t.Errorf("%s variable flags = %q, want %q", inv.args[0], got, wantInv)
				}
			}
		})
	}
}

// TestRunner_NoCheckoutNeeded verifies that the runner is an ordinary
// executable: it runs with no BUILD_WORKSPACE_DIRECTORY and from an unrelated
// working directory, and tofu still runs in the deploy's run directory. setup
// removes BUILD_WORKSPACE_DIRECTORY to match running a built launcher directly
// rather than through `bazel run`.
func TestRunner_NoCheckoutNeeded(t *testing.T) {
	cmd, _, invocations := setup(t)
	for _, e := range cmd.Env {
		if strings.HasPrefix(e, "BUILD_WORKSPACE_DIRECTORY=") {
			t.Fatalf("setup left %s in the runner's environment", e)
		}
	}
	cmd.Dir = t.TempDir()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	invs := invocations()
	if len(invs) != 2 {
		t.Fatalf("expected 2 tofu invocations (init, plan), got %d: %+v", len(invs), invs)
	}
	for _, inv := range invs {
		assertSameDir(t, inv.cwd, filepath.Join(runDir(t, cmd), "mypkg"))
	}
}

// TestRunner_MissingRequiredFlags verifies that omitting any single required
// flag causes the runner to exit non-zero with a message naming the flag.
func TestRunner_MissingRequiredFlags(t *testing.T) {
	requiredFlags := []string{
		"--tofu",
		"--work-tree",
		"--command",
		"--state-dir",
		"--output-root",
		"--plugin-dir",
		"--modules-manifest",
	}
	for _, flagName := range requiredFlags {
		flagName := flagName
		t.Run(flagName, func(t *testing.T) {
			cmd, _, _ := setup(t)
			filtered := cmd.Args[:1] // keep argv[0]
			for _, arg := range cmd.Args[1:] {
				if strings.HasPrefix(arg, flagName+"=") || arg == flagName {
					continue
				}
				filtered = append(filtered, arg)
			}
			cmd.Args = filtered
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("expected runner to fail when %s is omitted, but it succeeded", flagName)
			}
			if !strings.Contains(string(out), flagName) {
				t.Errorf("expected output to mention %s, got: %s", flagName, out)
			}
		})
	}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// argValue returns the value of the named --flag=value entry in the runner's
// args, failing the test if the flag is absent.
func argValue(t *testing.T, cmd *exec.Cmd, flagName string) string {
	t.Helper()
	prefix := flagName + "="
	for _, arg := range cmd.Args {
		if strings.HasPrefix(arg, prefix) {
			return strings.TrimPrefix(arg, prefix)
		}
	}
	t.Fatalf("could not find %s flag in runner cmd args", flagName)
	return ""
}

// envValue returns the value of the named variable in the runner's environment,
// failing the test if it is absent.
func envValue(t *testing.T, cmd *exec.Cmd, name string) string {
	t.Helper()
	prefix := name + "="
	for _, e := range cmd.Env {
		if strings.HasPrefix(e, prefix) {
			return strings.TrimPrefix(e, prefix)
		}
	}
	t.Fatalf("could not find %s in runner cmd env", name)
	return ""
}

// assertRefused runs cmd and checks that it exits non-zero containing every
// substring in wants after running tofu init and no further tofu command.
func assertRefused(t *testing.T, cmd *exec.Cmd, wants ...string) {
	t.Helper()
	logPath := envValue(t, cmd, "TOFU_LOG")
	if err := os.Remove(logPath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected the runner to refuse, but it succeeded:\n%s", out)
	}
	for _, want := range wants {
		if !strings.Contains(string(out), want) {
			t.Errorf("expected the refusal to contain %q, got: %s", want, out)
		}
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read tofu invocation log: %v", err)
	}
	if invs := parseInvocations(data); len(invs) != 1 || !invs[0].hasArg("init") {
		t.Errorf("expected init and nothing after it, got: %+v", invs)
	}
}

// assertRefusedEphemeralState checks the refusal for a deploy with no backend
// or cloud block, verifying that it names the run-directory state path and
// allow_ephemeral_state = True.
func assertRefusedEphemeralState(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	assertRefused(t, cmd,
		"declares no state backend",
		"terraform.tfstate",
		filepath.Join(runDir(t, cmd), "mypkg"),
		"allow_ephemeral_state = True",
	)
}

// When tofu init writes no backend record, apply and destroy are refused
// unless --allow-ephemeral-state is set, while plan runs ungated.
func TestRunner_EphemeralState_NoBackend(t *testing.T) {
	cmd, _, invocations := setup(t)
	for _, command := range []string{"apply", "destroy"} {
		assertRefusedEphemeralState(t, withCommand(cmd, command))
	}
	if out, err := withCommand(cmd, "plan").CombinedOutput(); err != nil {
		t.Fatalf("plan should not be gated, but the runner failed: %v\n%s", err, out)
	}
	for _, command := range []string{"apply", "destroy"} {
		if out, err := withCommand(cmd, command, "--allow-ephemeral-state").CombinedOutput(); err != nil {
			t.Fatalf("%s failed unexpectedly with --allow-ephemeral-state: %v\n%s", command, err, out)
		}
		invs := invocations()
		if last := invs[len(invs)-1]; !last.hasArg(command) {
			t.Errorf("expected %s to run with --allow-ephemeral-state, last invocation: %v", command, last.args)
		}
	}
}

// A backend record with a null, missing or empty backend is treated as having
// no backend; OpenTofu writes such a record when migrating off a backend.
func TestRunner_EphemeralState_EmptyRecord(t *testing.T) {
	for name, record := range map[string]string{
		"null":    `{"version": 3, "backend": null}`,
		"absent":  `{"version": 3}`,
		"no type": `{"version": 3, "backend": {"type": "", "config": null}}`,
	} {
		t.Run(name, func(t *testing.T) {
			cmd, _, _ := setup(t)
			setBackendRecord(cmd, record)
			assertRefusedEphemeralState(t, withCommand(cmd, "apply"))
		})
	}
}

// Any recorded backend type other than "local", including "cloud" for a cloud
// block, allows apply and destroy without --allow-ephemeral-state.
func TestRunner_EphemeralState_RemoteBackendAllowed(t *testing.T) {
	for typ, config := range map[string]map[string]any{
		"s3":    {"bucket": "my-tofu-state", "key": "prod"},
		"cloud": {"hostname": "app.example.com", "organization": "acme", "workspaces": map[string]any{"name": "prod"}},
	} {
		t.Run(typ, func(t *testing.T) {
			cmd, _, invocations := setup(t)
			setBackendRecord(cmd, backendRecord(t, typ, config))
			for _, command := range []string{"apply", "destroy"} {
				if out, err := withCommand(cmd, command).CombinedOutput(); err != nil {
					t.Fatalf("%s failed unexpectedly with a %s backend recorded: %v\n%s", command, typ, err, out)
				}
				invs := invocations()
				if last := invs[len(invs)-1]; !last.hasArg(command) {
					t.Errorf("expected %s to run, last invocation: %v", command, last.args)
				}
			}
		})
	}
}

// Removing the previous run's backend record before tofu init ensures a
// deploy whose backend block was deleted is checked against the new init run
// rather than the stale record.
func TestRunner_EphemeralState_StaleRecordRemoved(t *testing.T) {
	cmd, _, _ := setup(t)
	record := recordPath(t, cmd)
	if err := os.MkdirAll(filepath.Dir(record), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(record, []byte(backendRecord(t, "s3", map[string]any{"bucket": "b"})), 0o644); err != nil {
		t.Fatal(err)
	}
	// The fake tofu's init fails if the record is still there when it runs.
	setFakeTofu(t, cmd, `if [ "$1" = init ] && [ -e "$TF_DATA_DIR/terraform.tfstate" ] && [ -z "${TOFU_BACKEND_RECORD-}" ]; then echo "stale record" >&2; exit 1; fi
`)
	assertRefusedEphemeralState(t, withCommand(cmd, "apply"))
	if _, err := os.Lstat(record); !os.IsNotExist(err) {
		t.Errorf("the previous run's backend record should be gone, got err=%v", err)
	}
}

// A malformed backend record refuses apply unless --allow-ephemeral-state
// bypasses the check.
func TestRunner_EphemeralState_UnreadableRecord(t *testing.T) {
	for name, record := range map[string]string{
		"not json":           `{"version": 3, "backend": {`,
		"local, bad setting": `{"version": 3, "backend": {"type": "local", "config": {"path": 7}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			cmd, _, _ := setup(t)
			setBackendRecord(cmd, record)
			assertRefused(t, withCommand(cmd, "apply"), "can't tell where")
			if out, err := withCommand(cmd, "apply", "--allow-ephemeral-state").CombinedOutput(); err != nil {
				t.Fatalf("apply failed unexpectedly with --allow-ephemeral-state: %v\n%s", err, out)
			}
		})
	}
}

// When tofu init fails and terraform.tfstate in the run directory is non-zero
// in size, the runner logs a conditional migration hint naming that file and
// tofu init -migrate-state, covering -input=false blocking OpenTofu's prompt.
func TestRunner_InitFailure_NamesLocalState(t *testing.T) {
	const hint = "tofu init -migrate-state"
	cmd, _, _ := setup(t)
	setFakeTofu(t, cmd, "exit 1\n")
	out, err := withCommand(cmd, "plan").CombinedOutput()
	if err == nil || strings.Contains(string(out), hint) {
		t.Fatalf("expected init to fail without the migration hint, got err=%v:\n%s", err, out)
	}
	state := filepath.Join(runDir(t, cmd), "mypkg", "terraform.tfstate")
	if err := os.WriteFile(state, []byte(`{"version": 4}`), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = withCommand(cmd, "plan").CombinedOutput()
	if err == nil || !strings.Contains(string(out), hint) || !strings.Contains(string(out), state) {
		t.Errorf("expected init to fail naming %s and %q, got err=%v:\n%s", state, hint, err, out)
	}
}

// TestRunner_EmptyPackageDir_RunsAtRunDirRoot verifies that an empty
// --package-dir is valid: it models a deploy in the workspace root package, so
// tofu runs with the run directory's root itself as its working directory. The
// BCR test module e2e/smoke has this shape.
func TestRunner_EmptyPackageDir_RunsAtRunDirRoot(t *testing.T) {
	cmd, _, invocations := setupPkg(t, "")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly with empty --package-dir: %v\n%s", err, out)
	}
	invs := invocations()
	if len(invs) != 2 {
		t.Fatalf("expected 2 tofu invocations (init, plan), got %d: %+v", len(invs), invs)
	}
	if !invs[0].hasArg("init") {
		t.Errorf("first invocation should be 'init', got args: %v", invs[0].args)
	}
	if !invs[1].hasArg("plan") {
		t.Errorf("second invocation should be 'plan', got args: %v", invs[1].args)
	}
	for _, inv := range invs {
		assertSameDir(t, inv.cwd, runDir(t, cmd))
	}
	assertLink(t, filepath.Join(runDir(t, cmd), "variables.tf"), filepath.Join(argValue(t, cmd, "--work-tree"), "variables.tf"))
}

// TestRunner_LockFile_CopiedBeforeInit verifies that --lock-file lands at
// .terraform.lock.hcl in the root module's run directory, replacing a lock left
// by an earlier run, so init checks against the pinned lock.
func TestRunner_LockFile_CopiedBeforeInit(t *testing.T) {
	cmd, _, invocations := setup(t)
	cwd := filepath.Join(runDir(t, cmd), "mypkg")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(cwd, ".terraform.lock.hcl")
	if err := os.WriteFile(stale, []byte("# stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(t.TempDir(), ".terraform.lock.hcl")
	const want = "provider \"registry.opentofu.org/hashicorp/null\" {\n  version = \"3.2.4\"\n}\n"
	if err := os.WriteFile(lock, []byte(want), 0o444); err != nil {
		t.Fatal(err)
	}
	cmd.Args = append(cmd.Args, "--lock-file="+lock)

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	got, err := os.ReadFile(stale)
	if err != nil {
		t.Fatalf("read copied lock: %v", err)
	}
	if string(got) != want {
		t.Errorf("lock in the run directory = %q, want %q", got, want)
	}
	if invs := invocations(); len(invs) == 0 || !invs[0].hasArg("init") {
		t.Errorf("expected init to run, got %+v", invs)
	}
	assertWorkTreeUntouched(t, cmd)
}

// TestRunner_NoLockFile_RemovesLeftoverLock verifies that without --lock-file
// the runner writes no lock of its own and removes the one tofu wrote on an
// earlier run: it records that build's provider versions, and would make init
// refuse a provider bumped since.
func TestRunner_NoLockFile_RemovesLeftoverLock(t *testing.T) {
	cmd, _, _ := setup(t)
	cwd := filepath.Join(runDir(t, cmd), "mypkg")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(cwd, ".terraform.lock.hcl")
	if err := os.WriteFile(lock, []byte("# from an earlier run\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	if _, err := os.Lstat(lock); !os.IsNotExist(err) {
		t.Errorf("expected no lock at %s, got err=%v", lock, err)
	}
	assertWorkTreeUntouched(t, cmd)
}

// setupTest builds a `--command=test` invocation matching a generated tf_test
// launcher. It sets TEST_TMPDIR and TEST_UNDECLARED_OUTPUTS_DIR and omits
// --state-dir and --output-root. The fake tofu logs its calls and writes
// errored_test.tfstate on `test` when FAKE_ERRORED is set. It returns the
// command, the work tree in runfiles, TEST_TMPDIR, the outputs directory and
// a reader for recorded tofu calls.
func setupTest(t *testing.T) (cmd *exec.Cmd, workTree, scratch, outputs string, invocations func() []invocation) {
	t.Helper()
	bin := runnerBin(t)
	dir := t.TempDir()

	logPath := filepath.Join(dir, "tofu-invocations.log")
	fakeTofuSh := filepath.Join(dir, "tofu.sh")
	script := fakeTofuScript("if [ \"$1\" = test ] && [ -n \"${FAKE_ERRORED:-}\" ]; then echo '{}' > errored_test.tfstate; fi\n")
	if err := os.WriteFile(fakeTofuSh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	// The work tree as runfiles hold it: symlinks to the sources, at their
	// workspace-relative paths.
	sources := filepath.Join(dir, "sources")
	workTree = filepath.Join(dir, "work")
	if err := os.MkdirAll(filepath.Join(workTree, "mypkg", "tests"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"mypkg/main.tf", "mypkg/tests/t.tftest.hcl", "mypkg/test.tfvars"} {
		src := filepath.Join(sources, f)
		if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(src, []byte("# "+f+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(src, filepath.Join(workTree, f)); err != nil {
			t.Fatal(err)
		}
	}

	modulesManifest := filepath.Join(dir, "modules.json")
	if err := os.WriteFile(modulesManifest, []byte(testManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	scratch = filepath.Join(dir, "tmp")
	outputs = filepath.Join(dir, "outputs")
	for _, d := range []string{scratch, outputs} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	invocations = func() []invocation {
		t.Helper()
		data, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatalf("read tofu invocation log: %v", err)
		}
		return parseInvocations(data)
	}

	cmd = exec.Command(bin,
		"--tofu="+fakeTofuSh,
		"--work-tree="+workTree,
		"--package-dir=mypkg",
		"--plugin-dir="+filepath.Join(workTree, "mypkg.plugins"),
		"--modules-manifest="+modulesManifest,
		"--command=test",
		"--test-dir=tests",
		"--var-file="+filepath.Join(workTree, "mypkg", "test.tfvars"),
		"--var=name=a b",
	)
	cmd.Env = append(os.Environ(),
		"TOFU_LOG="+logPath,
		"TEST_TMPDIR="+scratch,
		"TEST_UNDECLARED_OUTPUTS_DIR="+outputs,
	)
	return cmd, workTree, scratch, outputs, invocations
}

// testRunDir returns the <TEST_TMPDIR>/rules_tofu-test-*/work directory
// created by a `--command=test` run.
func testRunDir(t *testing.T, scratch string) string {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join(scratch, "rules_tofu-test-*", "work"))
	if err != nil || len(dirs) != 1 {
		t.Fatalf("run directories under %s = %v (err=%v), want one", scratch, dirs, err)
	}
	return dirs[0]
}

// `--command=test` mirrors the work tree as symlinks under TEST_TMPDIR. In
// the run directory there it runs `tofu init` offline with -backend=false and
// -test-directory. It then runs `tofu test` with -json-into in the undeclared
// outputs. Both commands receive -var and -var-file. Trailing arguments go to
// `tofu test` last. The runfiles work tree is left untouched.
func TestRunner_Test_InitsAndTestsALinkedTree(t *testing.T) {
	cmd, workTree, scratch, outputs, invocations := setupTest(t)
	cmd.Args = append(cmd.Args, "--", "-verbose")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	invs := invocations()
	if len(invs) != 2 {
		t.Fatalf("got %d tofu invocations, want init then test: %+v", len(invs), invs)
	}
	runPkg := filepath.Join(testRunDir(t, scratch), "mypkg")
	// The fake tofu logs $(pwd), a resolved path: on macOS /var is a symlink
	// to /private/var.
	wantCwd, err := filepath.EvalSymlinks(runPkg)
	if err != nil {
		t.Fatal(err)
	}
	init, test := invs[0], invs[1]
	if init.cwd != wantCwd || test.cwd != wantCwd {
		t.Errorf("cwd = %q and %q, want %q", init.cwd, test.cwd, wantCwd)
	}
	for _, arg := range []string{"init", "-backend=false", "-input=false", "-get=false", "-test-directory=tests"} {
		if !init.hasArg(arg) {
			t.Errorf("init lacks %q: %v", arg, init.args)
		}
	}
	if !init.hasArgWithPrefix("-plugin-dir=") {
		t.Errorf("init lacks -plugin-dir: %v", init.args)
	}
	varArgs := []string{"-var=name=a b", "-var-file=" + filepath.Join(workTree, "mypkg", "test.tfvars")}
	if got := init.args[len(init.args)-len(varArgs):]; !reflect.DeepEqual(got, varArgs) {
		t.Errorf("init args end %q, want %q", got, varArgs)
	}
	wantTest := append([]string{"test", "-no-color", "-test-directory=tests", "-json-into=" + filepath.Join(outputs, "tofu-test.json")}, varArgs...)
	wantTest = append(wantTest, "-verbose")
	if !reflect.DeepEqual(test.args, wantTest) {
		t.Errorf("test args = %q, want %q", test.args, wantTest)
	}
	// The run directory links each work-tree file and holds a regular
	// manifest file. The runfiles tree is untouched.
	for _, f := range []string{"main.tf", "tests/t.tftest.hcl", "test.tfvars"} {
		assertLink(t, filepath.Join(runPkg, f), filepath.Join(workTree, "mypkg", f))
	}
	if data, err := os.ReadFile(filepath.Join(runPkg, "tests", "t.tftest.hcl")); err != nil || string(data) != "# mypkg/tests/t.tftest.hcl\n" {
		t.Errorf("linked test file = %q, %v", data, err)
	}
	if info, err := os.Lstat(filepath.Join(runPkg, ".terraform", "modules", "modules.json")); err != nil || !info.Mode().IsRegular() {
		t.Errorf("the manifest is not a regular file in the run directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workTree, "mypkg", ".terraform")); !os.IsNotExist(err) {
		t.Errorf("runfiles work tree gained .terraform: err=%v", err)
	}
}

// The lock is copied into the run directory only when the root module has
// configuration. In an empty root module `tofu init` installs no providers.
// Leaving the lock there would fail validation and tests against an empty
// .terraform/providers directory.
func TestRunner_Test_LockOnlyWithConfiguration(t *testing.T) {
	for name, empty := range map[string]bool{"configuration": false, "an empty root": true} {
		t.Run(name, func(t *testing.T) {
			cmd, workTree, scratch, _, _ := setupTest(t)
			if empty {
				if err := os.Remove(filepath.Join(workTree, "mypkg", "main.tf")); err != nil {
					t.Fatal(err)
				}
			}
			lock := filepath.Join(t.TempDir(), ".terraform.lock.hcl")
			if err := os.WriteFile(lock, []byte("# lock\n"), 0o444); err != nil {
				t.Fatal(err)
			}
			cmd.Args = append(cmd.Args, "--lock-file="+lock)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
			}
			_, err := os.Stat(filepath.Join(testRunDir(t, scratch), "mypkg", ".terraform.lock.hcl"))
			if empty && !os.IsNotExist(err) {
				t.Errorf("an empty root module was given the lock: err=%v", err)
			}
			if !empty && err != nil {
				t.Errorf("the root module wasn't given the lock: %v", err)
			}
		})
	}
}

// When no providers are in scope, the plugin tree is absent from runfiles.
// tofu receives an empty fallback directory under TEST_TMPDIR. Nothing is
// created in runfiles.
func TestRunner_Test_MissingPluginDirFallsBack(t *testing.T) {
	cmd, workTree, scratch, _, invocations := setupTest(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	pluginDir := strings.TrimPrefix(argValue(t, cmd, "--plugin-dir"), "")
	if _, err := os.Stat(pluginDir); !os.IsNotExist(err) {
		t.Errorf("plugin dir %s was created in runfiles: err=%v", pluginDir, err)
	}
	init := invocations()[0]
	var got string
	for _, a := range init.args {
		if strings.HasPrefix(a, "-plugin-dir=") {
			got = strings.TrimPrefix(a, "-plugin-dir=")
		}
	}
	if want := filepath.Join(filepath.Dir(testRunDir(t, scratch)), "plugins"); got != want {
		t.Errorf("-plugin-dir = %q, want %q", got, want)
	}
	if info, err := os.Stat(got); err != nil || !info.IsDir() {
		t.Errorf("fallback plugin dir: %v", err)
	}
	_ = workTree
}

// A plugin tree that is present is used in place.
func TestRunner_Test_PluginDirUsedInPlace(t *testing.T) {
	cmd, _, _, _, invocations := setupTest(t)
	pluginDir := argValue(t, cmd, "--plugin-dir")
	if err := os.MkdirAll(filepath.Join(pluginDir, "registry.opentofu.org"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	if init := invocations()[0]; !init.hasArg("-plugin-dir=" + pluginDir) {
		t.Errorf("init args = %v, want -plugin-dir=%s", init.args, pluginDir)
	}
}

// An errored_test.tfstate left by a failed destroy fails the test, even
// though tofu exited 0, and is kept with the test's outputs.
func TestRunner_Test_ErroredStateFails(t *testing.T) {
	cmd, _, _, outputs, _ := setupTest(t)
	cmd.Env = append(cmd.Env, "FAKE_ERRORED=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("runner succeeded despite errored_test.tfstate:\n%s", out)
	}
	kept := filepath.Join(outputs, "errored_test.tfstate")
	if !strings.Contains(string(out), "left resources in state") || !strings.Contains(string(out), kept) {
		t.Errorf("output = %q, want the errored-state message naming %s", out, kept)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("errored state not kept: %v", err)
	}
}

// A launcher run directly has neither TEST_TMPDIR nor
// TEST_UNDECLARED_OUTPUTS_DIR. Without errored_test.tfstate, the temporary
// directory under TMPDIR is removed on exit. When errored_test.tfstate is
// written, the directory is kept so the path named in the error exists.
func TestRunner_Test_ErroredStateKeptWithoutOutputs(t *testing.T) {
	cmd, _, _, _, _ := setupTest(t)
	tmp := t.TempDir()
	var env []string
	for _, e := range cmd.Env {
		if !strings.HasPrefix(e, "TEST_TMPDIR=") && !strings.HasPrefix(e, "TEST_UNDECLARED_OUTPUTS_DIR=") && !strings.HasPrefix(e, "TMPDIR=") {
			env = append(env, e)
		}
	}
	cmd.Env = append(env, "TMPDIR="+tmp, "FAKE_ERRORED=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("runner succeeded despite errored_test.tfstate:\n%s", out)
	}
	_, kept, found := strings.Cut(strings.TrimSpace(string(out)), "state file kept at ")
	if !found || !strings.HasPrefix(kept, tmp) {
		t.Fatalf("output = %q, want the errored-state message naming a file under %s", out, tmp)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("the errored state the message names was removed: %v", err)
	}

	// A run that leaves no errored state removes its directory.
	pass := exec.Command(cmd.Args[0], cmd.Args[1:]...)
	other := t.TempDir()
	pass.Env = append(env, "TMPDIR="+other)
	if out, err := pass.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	if left, err := os.ReadDir(other); err != nil || len(left) != 0 {
		t.Errorf("TMPDIR holds %v after the run (err=%v), want nothing", left, err)
	}
}

// `--command=test` needs no --state-dir; plan, apply and destroy still do.
func TestRunner_Test_StateDirNotRequired(t *testing.T) {
	cmd, _, _, _, _ := setupTest(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	for i, a := range cmd.Args {
		if a == "--command=test" {
			cmd.Args[i] = "--command=plan"
		}
	}
	plan := exec.Command(cmd.Args[0], cmd.Args[1:]...)
	plan.Env = cmd.Env
	out, err := plan.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "--state-dir is required") {
		t.Errorf("plan without --state-dir: err=%v out=%q", err, out)
	}
}

// Each run removes the previous .terraform/terraform.tfstate before tofu init
// so edited backend blocks take effect without passing -reconfigure, which
// OpenTofu rejects when a cloud block is present.
func TestRunner_Init_FreshBackend(t *testing.T) {
	cmd, _, invocations := setup(t)
	setBackendRecord(cmd, backendRecord(t, "s3", map[string]any{"bucket": "old"}))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	if got, err := os.ReadFile(recordPath(t, cmd)); err != nil || !strings.Contains(string(got), "old") {
		t.Fatalf("the fake tofu should have recorded the backend: %q, %v", got, err)
	}
	// The second init must find no record.
	setFakeTofu(t, cmd, `if [ "$1" = init ] && [ -e "$TF_DATA_DIR/terraform.tfstate" ]; then echo "record left in place" >&2; exit 1; fi
`)
	if out, err := withCommand(cmd, "plan").CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	for _, inv := range invocations() {
		if inv.hasArg("init") && (inv.hasArg("-reconfigure") || inv.hasArg("-migrate-state") || inv.hasArg("-backend=false")) {
			t.Errorf("init args = %v, want no -reconfigure, -migrate-state or -backend=false", inv.args)
		}
	}
}

// tofu runs with TF_DATA_DIR fixed to the run directory's `.terraform` and no
// TF_PLUGIN_CACHE_DIR, whatever the parent environment holds.
func TestRunner_Env_DataDirAndNoPluginCache(t *testing.T) {
	cmd, _, invocations := setup(t)
	cmd.Env = append(cmd.Env, "TF_DATA_DIR=/bogus/data", "TF_PLUGIN_CACHE_DIR=/bogus/cache")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	want := filepath.Join(runDir(t, cmd), "mypkg", ".terraform")
	for _, inv := range invocations() {
		if inv.env["TF_DATA_DIR"] != want {
			t.Errorf("%s: TF_DATA_DIR = %q, want %q", inv.args[0], inv.env["TF_DATA_DIR"], want)
		}
		if inv.env["TF_PLUGIN_CACHE_DIR"] != "<unset>" {
			t.Errorf("%s: TF_PLUGIN_CACHE_DIR = %q, want it unset", inv.args[0], inv.env["TF_PLUGIN_CACHE_DIR"])
		}
	}
}

// In test mode, tofu runs with TF_DATA_DIR set to the run directory's
// .terraform and TF_PLUGIN_CACHE_DIR unset. `tofu init` passes -backend=false
// without -reconfigure.
func TestRunner_Test_EnvAndInitFlags(t *testing.T) {
	cmd, _, scratch, _, invocations := setupTest(t)
	cmd.Env = append(cmd.Env, "TF_DATA_DIR=/bogus/data", "TF_PLUGIN_CACHE_DIR=/bogus/cache")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	invs := invocations()
	if len(invs) != 2 {
		t.Fatalf("got %d tofu invocations, want init then test: %+v", len(invs), invs)
	}
	if init := invs[0]; init.hasArg("-reconfigure") || !init.hasArg("-backend=false") {
		t.Errorf("test-mode init args = %v, want -backend=false and no -reconfigure", init.args)
	}
	want := filepath.Join(testRunDir(t, scratch), "mypkg", ".terraform")
	for _, inv := range invs {
		if inv.env["TF_DATA_DIR"] != want {
			t.Errorf("%s: TF_DATA_DIR = %q, want %q", inv.args[0], inv.env["TF_DATA_DIR"], want)
		}
		if inv.env["TF_PLUGIN_CACHE_DIR"] != "<unset>" {
			t.Errorf("%s: TF_PLUGIN_CACHE_DIR = %q, want it unset", inv.args[0], inv.env["TF_PLUGIN_CACHE_DIR"])
		}
	}
}

// runDir returns the deploy's run directory, <state-dir>/work, which mirrors
// the work tree.
func runDir(t *testing.T, cmd *exec.Cmd) string {
	t.Helper()
	return filepath.Join(argValue(t, cmd, "--state-dir"), "work")
}

// setFakeTofu replaces the fake tofu with one that also runs extra as shell
// commands in tofu's working directory, with the subcommand in $1.
func setFakeTofu(t *testing.T, cmd *exec.Cmd, extra string) {
	t.Helper()
	if err := os.WriteFile(argValue(t, cmd, "--tofu"), []byte(fakeTofuScript(extra)), 0o755); err != nil {
		t.Fatal(err)
	}
}

// withCommand returns a fresh copy of cmd with its --command replaced and the
// given extra flags added. An exec.Cmd runs once.
func withCommand(cmd *exec.Cmd, command string, flags ...string) *exec.Cmd {
	args := []string{}
	for _, a := range cmd.Args[1:] {
		if !strings.HasPrefix(a, "--command=") {
			args = append(args, a)
		}
	}
	args = append(args, "--command="+command)
	c := exec.Command(cmd.Args[0], append(args, flags...)...)
	c.Env = cmd.Env
	c.Dir = cmd.Dir
	return c
}

// assertSameDir asserts that got and want name the same directory. Symlinks
// are resolved on both sides: the fake tofu records the physical path, while
// the flags carry the logical one.
func assertSameDir(t *testing.T, got, want string) {
	t.Helper()
	g, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatalf("resolve %q: %v", got, err)
	}
	w, err := filepath.EvalSymlinks(want)
	if err != nil {
		t.Fatalf("resolve %q: %v", want, err)
	}
	if g != w {
		t.Errorf("directory = %q, want %q", g, w)
	}
}

// assertLink asserts that link is a symlink whose target is exactly target.
func assertLink(t *testing.T, link, target string) {
	t.Helper()
	got, err := os.Readlink(link)
	if err != nil {
		t.Errorf("%s should be a symlink into the work tree: %v", link, err)
		return
	}
	if got != target {
		t.Errorf("%s links to %q, want %q", link, got, target)
	}
}

// assertWorkTreeUntouched asserts that the run left nothing of tofu's in the
// work tree's package directory: tofu runs in the run directory, and the work
// tree is Bazel's.
func assertWorkTreeUntouched(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	pkg := filepath.Join(argValue(t, cmd, "--work-tree"), argValue(t, cmd, "--package-dir"))
	for _, name := range []string{".terraform", ".terraform.lock.hcl", "terraform.tfstate"} {
		if _, err := os.Lstat(filepath.Join(pkg, name)); !os.IsNotExist(err) {
			t.Errorf("%s appeared in the work tree (err=%v)", filepath.Join(pkg, name), err)
		}
	}
}

// writeWorkTreeFile writes a file at the given path relative to the work tree
// root, creating its directory, and returns the path written.
func writeWorkTreeFile(t *testing.T, cmd *exec.Cmd, rel, content string) string {
	t.Helper()
	path := filepath.Join(argValue(t, cmd, "--work-tree"), rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// tofu runs in `<state-dir>/work/<package-dir>` for plan, apply and destroy
// alike: one directory per deploy, mirroring the work tree file for file with
// absolute symlinks, so the root module, its data files and the modules beside
// it are all where tofu expects them. Nothing is written into the work tree.
func TestRunner_RunDir_MirrorsWorkTree(t *testing.T) {
	cmd, _, invocations := setup(t)
	// The lock and the data directory are what tofu writes beside the
	// configuration.
	setFakeTofu(t, cmd, ": > .terraform.lock.hcl; mkdir -p .terraform/providers\n")
	files := []string{
		"mypkg/variables.tf",
		"mypkg/main.tf",
		"mypkg/templates/a.tftpl",
		"modules/net/main.tf",
	}
	for _, f := range files[1:] {
		writeWorkTreeFile(t, cmd, f, "# "+f+"\n")
	}

	for _, command := range []string{"plan", "apply", "destroy"} {
		if out, err := withCommand(cmd, command, "--allow-ephemeral-state").CombinedOutput(); err != nil {
			t.Fatalf("%s failed unexpectedly: %v\n%s", command, err, out)
		}
	}
	invs := invocations()
	if len(invs) != 6 {
		t.Fatalf("expected 6 tofu invocations (init and the command, three times), got %d: %+v", len(invs), invs)
	}
	for _, inv := range invs {
		assertSameDir(t, inv.cwd, filepath.Join(runDir(t, cmd), "mypkg"))
	}
	for _, f := range files {
		assertLink(t, filepath.Join(runDir(t, cmd), f), filepath.Join(argValue(t, cmd, "--work-tree"), f))
	}
	assertWorkTreeUntouched(t, cmd)
}

// Paths given relative to the runner's own working directory are made
// absolute before tofu, which runs elsewhere, sees them, and before they
// become link targets.
func TestRunner_RunDir_RelativeFlags(t *testing.T) {
	cmd, addVarFile, invocations := setup(t)
	base := filepath.Dir(argValue(t, cmd, "--work-tree"))
	// The runner makes them absolute against its working directory as the
	// kernel names it, which is not always how t.TempDir() spells it. On macOS
	// /var is a link to /private/var.
	physBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Args = append(cmd.Args, "--var-file="+addVarFile(map[string]any{"env": "prod"}))
	if err := os.MkdirAll(argValue(t, cmd, "--plugin-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{}
	for i, a := range cmd.Args {
		for _, name := range []string{"--tofu", "--work-tree", "--plugin-dir", "--state-dir", "--output-root", "--var-file"} {
			if v, ok := strings.CutPrefix(a, name+"="); ok {
				rel, err := filepath.Rel(base, v)
				if err != nil {
					t.Fatal(err)
				}
				cmd.Args[i] = name + "=" + rel
				want[name] = filepath.Join(physBase, rel)
			}
		}
	}
	cmd.Dir = base
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	invs := invocations()
	if len(invs) != 2 {
		t.Fatalf("expected 2 tofu invocations (init, plan), got %d: %+v", len(invs), invs)
	}
	if arg := "-plugin-dir=" + want["--plugin-dir"]; !invs[0].hasArg(arg) {
		t.Errorf("init args = %v, want %s", invs[0].args, arg)
	}
	if arg := "-var-file=" + want["--var-file"]; !invs[1].hasArg(arg) {
		t.Errorf("plan args = %v, want %s", invs[1].args, arg)
	}
	assertLink(t,
		filepath.Join(want["--state-dir"], "work", "mypkg", "variables.tf"),
		filepath.Join(want["--work-tree"], "mypkg", "variables.tf"))
}

// The run directory follows the work tree from one run to the next: a file
// that left the work tree loses its link, a new one gains a link, and what
// tofu wrote there is kept so the command after sees it. Here that is
// terraform.tfstate from a deploy with no backend block; relative local
// backend paths are preserved the same way (issue #86).
func TestRunner_RunDir_FollowsWorkTreeAndKeepsState(t *testing.T) {
	cmd, _, _ := setup(t)
	// apply writes terraform.tfstate in cwd; destroy fails unless it stays.
	setFakeTofu(t, cmd, `case "$1" in
  apply) echo applied > terraform.tfstate ;;
  destroy) [ "$(cat terraform.tfstate)" = applied ] || { echo "no state" >&2; exit 1; } ;;
esac
`)
	gone := writeWorkTreeFile(t, cmd, "mypkg/gone.tf", "# gone\n")
	if out, err := withCommand(cmd, "apply", "--allow-ephemeral-state").CombinedOutput(); err != nil {
		t.Fatalf("apply failed unexpectedly: %v\n%s", err, out)
	}
	cwd := filepath.Join(runDir(t, cmd), "mypkg")
	assertLink(t, filepath.Join(cwd, "gone.tf"), gone)

	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	added := writeWorkTreeFile(t, cmd, "mypkg/added.tf", "# added\n")
	if out, err := withCommand(cmd, "destroy", "--allow-ephemeral-state").CombinedOutput(); err != nil {
		t.Fatalf("destroy failed unexpectedly: %v\n%s", err, out)
	}
	if _, err := os.Lstat(filepath.Join(cwd, "gone.tf")); !os.IsNotExist(err) {
		t.Errorf("the link to a file no longer in the work tree should be gone, got err=%v", err)
	}
	assertLink(t, filepath.Join(cwd, "added.tf"), added)
	if got, err := os.ReadFile(filepath.Join(cwd, "terraform.tfstate")); err != nil || string(got) != "applied\n" {
		t.Errorf("state in the run directory = %q, %v; want it kept", got, err)
	}
	assertWorkTreeUntouched(t, cmd)
}

// A regular file where a link must go is never overwritten: it is tofu's or
// the user's, and the runner stops before running tofu. An empty directory
// holds nothing and gives way.
func TestRunner_RunDir_FileInTheWay(t *testing.T) {
	cmd, _, _ := setup(t)
	writeWorkTreeFile(t, cmd, "mypkg/main.tf", "# main\n")
	writeWorkTreeFile(t, cmd, "mypkg/was_a_dir", "now a file\n")
	cwd := filepath.Join(runDir(t, cmd), "mypkg")
	if err := os.MkdirAll(filepath.Join(cwd, "was_a_dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	inTheWay := filepath.Join(cwd, "main.tf")
	if err := os.WriteFile(inTheWay, []byte("precious\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected the runner to stop at the file in the way, but it succeeded:\n%s", out)
	}
	if !strings.Contains(string(out), inTheWay) {
		t.Errorf("expected the error to name %s, got: %s", inTheWay, out)
	}
	if got, _ := os.ReadFile(inTheWay); string(got) != "precious\n" {
		t.Errorf("the file in the way was changed to %q", got)
	}
	if _, err := os.Stat(envValue(t, cmd, "TOFU_LOG")); !os.IsNotExist(err) {
		t.Errorf("expected no tofu invocations, got err=%v", err)
	}

	if err := os.Remove(inTheWay); err != nil {
		t.Fatal(err)
	}
	if out, err := withCommand(cmd, "plan").CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly once the file was moved away: %v\n%s", err, out)
	}
	assertLink(t, filepath.Join(cwd, "was_a_dir"), filepath.Join(argValue(t, cmd, "--work-tree"), "mypkg", "was_a_dir"))
}

// Of the .terraform/ an earlier run left, providers goes and the rest stays:
// providers links into that launcher's plugin tree and init relinks it, while
// the workspace selection in environment is the user's.
func TestRunner_RunDir_DataDirAcrossRuns(t *testing.T) {
	cmd, _, _ := setup(t)
	dataDir := filepath.Join(runDir(t, cmd), "mypkg", ".terraform")
	stale := filepath.Join(dataDir, "providers", "registry.opentofu.org", "hashicorp", "null")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nonexistent/plugins/null", stale); err != nil {
		t.Fatal(err)
	}
	environment := filepath.Join(dataDir, "environment")
	if err := os.WriteFile(environment, []byte("staging"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	if _, err := os.Lstat(filepath.Join(dataDir, "providers")); !os.IsNotExist(err) {
		t.Errorf("the previous run's .terraform/providers should be gone, got err=%v", err)
	}
	if got, err := os.ReadFile(environment); err != nil || string(got) != "staging" {
		t.Errorf(".terraform/environment = %q, %v; want it kept", got, err)
	}
}

// assertRefusedLocalBackend checks the refusal for a local backend that does
// not keep state outside Bazel's output tree, verifying that it names reason,
// the run directory and allow_ephemeral_state = True.
func assertRefusedLocalBackend(t *testing.T, cmd *exec.Cmd, reason string) {
	t.Helper()
	assertRefused(t, cmd,
		`backend "local" does not keep state outside Bazel's output tree`,
		reason,
		filepath.Join(runDir(t, cmd), "mypkg"),
		"allow_ephemeral_state = True",
	)
}

// A local backend's state is durable for apply and destroy only when both
// path and workspace_dir resolve to absolute paths outside Bazel's output
// tree after following symlinks, even when the target path does not exist yet.
// Null or empty attributes (which use OpenTofu's relative defaults), relative
// paths and paths inside the output tree are refused unless
// --allow-ephemeral-state is set. plan is never gated.
func TestRunner_LocalBackend_Gate(t *testing.T) {
	cases := []struct {
		name string
		// config builds the recorded local backend config given a directory
		// outside outputRoot, outputRoot itself and a symlink into outputRoot.
		config func(outside, outputRoot, link string) map[string]any
		// reason is why the state is ephemeral; empty when it is durable.
		reason string
	}{
		{"both absolute", func(o, _, _ string) map[string]any {
			return map[string]any{"path": o + "/prod.tfstate", "workspace_dir": o + "/workspaces"}
		}, ""},
		{"empty block", func(_, _, _ string) map[string]any {
			return map[string]any{"path": nil, "workspace_dir": nil}
		}, "its `path` is not set, and tofu's default for it (\"terraform.tfstate\") is relative"},
		{"empty path", func(o, _, _ string) map[string]any {
			return map[string]any{"path": "", "workspace_dir": o + "/workspaces"}
		}, "its `path` is not set, and tofu's default for it (\"terraform.tfstate\") is relative"},
		{"relative path", func(o, _, _ string) map[string]any {
			return map[string]any{"path": "state/dev.tfstate", "workspace_dir": o + "/workspaces"}
		}, "its `path` (\"state/dev.tfstate\") is relative"},
		{"workspace_dir unset", func(o, _, _ string) map[string]any {
			return map[string]any{"path": o + "/prod.tfstate", "workspace_dir": nil}
		}, "its `workspace_dir` is not set, and tofu's default for it (\"terraform.tfstate.d\") is relative"},
		{"empty workspace_dir", func(o, _, _ string) map[string]any {
			return map[string]any{"path": o + "/prod.tfstate", "workspace_dir": ""}
		}, "its `workspace_dir` is not set, and tofu's default for it (\"terraform.tfstate.d\") is relative"},
		{"relative workspace_dir", func(o, _, _ string) map[string]any {
			return map[string]any{"path": o + "/prod.tfstate", "workspace_dir": "ws"}
		}, "its `workspace_dir` (\"ws\") is relative"},
		{"inside the output tree", func(o, root, _ string) map[string]any {
			return map[string]any{"path": root + "/cfg/bin/pkg/prod.tfstate", "workspace_dir": o + "/workspaces"}
		}, "its `path` (\"%ROOT%/cfg/bin/pkg/prod.tfstate\") is inside Bazel's output tree"},
		{"workspace_dir inside the output tree", func(o, root, _ string) map[string]any {
			return map[string]any{"path": o + "/prod.tfstate", "workspace_dir": root + "/ws"}
		}, "its `workspace_dir` (\"%ROOT%/ws\") is inside Bazel's output tree"},
		// As through the `bazel-bin` link in a checkout, to a state file and a
		// directory that don't exist yet.
		{"through a symlink into the output tree", func(o, _, link string) map[string]any {
			return map[string]any{"path": link + "/not/yet/prod.tfstate", "workspace_dir": o + "/workspaces"}
		}, "is inside Bazel's output tree"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd, _, invocations := setup(t)
			base := filepath.Dir(argValue(t, cmd, "--work-tree"))
			outside := filepath.Join(base, "durable")
			outputRoot := argValue(t, cmd, "--output-root")
			link := filepath.Join(base, "bazel-bin")
			if err := os.MkdirAll(filepath.Join(outputRoot, "cfg", "bin"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(outputRoot, "cfg", "bin"), link); err != nil {
				t.Fatal(err)
			}
			setBackendRecord(cmd, backendRecord(t, "local", tc.config(outside, outputRoot, link)))
			reason := strings.ReplaceAll(tc.reason, "%ROOT%", outputRoot)

			// plan is never gated; apply and destroy run without the opt-in
			// when the state is durable, and with it otherwise.
			var flags []string
			if tc.reason != "" {
				for _, command := range []string{"apply", "destroy"} {
					assertRefusedLocalBackend(t, withCommand(cmd, command), reason)
				}
				flags = []string{"--allow-ephemeral-state"}
			}
			if out, err := withCommand(cmd, "plan").CombinedOutput(); err != nil {
				t.Fatalf("plan failed unexpectedly: %v\n%s", err, out)
			}
			for _, command := range []string{"apply", "destroy"} {
				if out, err := withCommand(cmd, command, flags...).CombinedOutput(); err != nil {
					t.Fatalf("%s failed unexpectedly: %v\n%s", command, err, out)
				}
				invs := invocations()
				if last := invs[len(invs)-1]; !last.hasArg(command) {
					t.Errorf("expected %s to run, last invocation: %v", command, last.args)
				}
			}
		})
	}
}

// A work tree holds files and real directories only. A symlink to a directory
// is refused by name for both tests and deploys (issue #89). WalkDir does not
// descend into one. Walking one by hand would loop on a link to an ancestor.
func TestRunner_Test_SymlinkToDirectoryFails(t *testing.T) {
	cmd, workTree, _, _, _ := setupTest(t)
	if err := os.Symlink(".", filepath.Join(workTree, "mypkg", "loop")); err != nil {
		t.Fatal(err)
	}
	assertDirectoryLinkRefused(t, cmd, "mypkg/loop")
}

func TestRunner_RunDir_SymlinkToDirectoryFails(t *testing.T) {
	cmd, _, _ := setup(t)
	writeWorkTreeFile(t, cmd, "mypkg/main.tf", "# main\n")
	writeWorkTreeFile(t, cmd, "modules/net/main.tf", "# net\n")
	workTree := argValue(t, cmd, "--work-tree")
	if err := os.Symlink(filepath.Join(workTree, "modules"), filepath.Join(workTree, "mypkg", "linked")); err != nil {
		t.Fatal(err)
	}
	assertDirectoryLinkRefused(t, cmd, "mypkg/linked")
}

// assertDirectoryLinkRefused runs the runner and checks that it fails naming
// the symlink at the work-tree path rel, before any tofu process starts.
func assertDirectoryLinkRefused(t *testing.T, cmd *exec.Cmd, rel string) {
	t.Helper()
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected the runner to refuse the symlink to a directory, but it succeeded:\n%s", out)
	}
	if want := filepath.FromSlash(rel) + " is a symlink to a directory"; !strings.Contains(string(out), want) {
		t.Errorf("expected the error to contain %q, got: %s", want, out)
	}
	if _, err := os.Stat(envValue(t, cmd, "TOFU_LOG")); !os.IsNotExist(err) {
		t.Errorf("expected no tofu invocations, got err=%v", err)
	}
}

// A test's run directory links to the read-only files in the work tree. An
// executable script runs through its symlink as a `local-exec` provisioner or
// `external` data source runs it. Files written beside the script land in the
// run directory rather than in the work tree or sources.
func TestRunner_Test_RunsScriptThroughLink(t *testing.T) {
	cmd, workTree, scratch, _, _ := setupTest(t)
	sources := filepath.Join(filepath.Dir(workTree), "sources", "mypkg")
	src := filepath.Join(sources, "run.sh")
	if err := os.WriteFile(src, []byte("#!/bin/sh\necho ran > \"$(dirname \"$0\")/ran.txt\"\n"), 0o555); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(src, filepath.Join(workTree, "mypkg", "run.sh")); err != nil {
		t.Fatal(err)
	}
	setFakeTofu(t, cmd, "if [ \"$1\" = test ]; then ./run.sh || exit 1; fi\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}

	runPkg := filepath.Join(testRunDir(t, scratch), "mypkg")
	assertLink(t, filepath.Join(runPkg, "run.sh"), filepath.Join(workTree, "mypkg", "run.sh"))
	if info, err := os.Lstat(filepath.Join(runPkg, "ran.txt")); err != nil || !info.Mode().IsRegular() {
		t.Errorf("the script did not write beside its link: %v", err)
	}
	for _, dir := range []string{sources, filepath.Join(workTree, "mypkg")} {
		if _, err := os.Lstat(filepath.Join(dir, "ran.txt")); !os.IsNotExist(err) {
			t.Errorf("the script wrote into %s: err=%v", dir, err)
		}
	}
}

// A tf_test launcher run directly by a relative path passes paths relative to
// the caller's working directory. The runner resolves them to absolute paths
// before passing them to tofu in the run directory or using them as symlink
// targets.
func TestRunner_Test_RelativeFlags(t *testing.T) {
	cmd, workTree, scratch, _, invocations := setupTest(t)
	base, err := filepath.EvalSymlinks(filepath.Dir(workTree))
	if err != nil {
		t.Fatal(err)
	}
	// Write the lock under setupTest's unresolved directory so filepath.Rel
	// below computes every flag against the same root. On macOS /var links
	// to /private/var.
	lock := filepath.Join(filepath.Dir(workTree), "lock.hcl")
	if err := os.WriteFile(lock, []byte("# lock\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	pluginDir := argValue(t, cmd, "--plugin-dir")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd.Args = append(cmd.Args, "--lock-file="+lock)
	for i, a := range cmd.Args {
		for _, name := range []string{"--tofu", "--work-tree", "--plugin-dir", "--modules-manifest", "--lock-file", "--var-file"} {
			if v, ok := strings.CutPrefix(a, name+"="); ok {
				rel, err := filepath.Rel(filepath.Dir(workTree), v)
				if err != nil {
					t.Fatal(err)
				}
				cmd.Args[i] = name + "=" + rel
			}
		}
	}
	cmd.Dir = base
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	invs := invocations()
	if len(invs) != 2 {
		t.Fatalf("got %d tofu invocations, want init then test: %+v", len(invs), invs)
	}
	for _, arg := range []string{"-plugin-dir=" + filepath.Join(base, "work", "mypkg.plugins"), "-var-file=" + filepath.Join(base, "work", "mypkg", "test.tfvars")} {
		if !invs[0].hasArg(arg) {
			t.Errorf("init args = %v, want %s", invs[0].args, arg)
		}
	}
	runPkg := filepath.Join(testRunDir(t, scratch), "mypkg")
	assertLink(t, filepath.Join(runPkg, "main.tf"), filepath.Join(base, "work", "mypkg", "main.tf"))
	if _, err := os.Stat(filepath.Join(runPkg, ".terraform.lock.hcl")); err != nil {
		t.Errorf("the lock was not installed: %v", err)
	}
}

// When no providers are in scope, a deploy's plugin tree is absent from
// runfiles. tofu receives an empty fallback directory under --state-dir.
// Nothing is created in runfiles.
func TestRunner_MissingPluginDirFallsBack(t *testing.T) {
	cmd, _, invocations := setup(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	pluginDir := argValue(t, cmd, "--plugin-dir")
	if _, err := os.Stat(pluginDir); !os.IsNotExist(err) {
		t.Errorf("plugin dir %s was created: err=%v", pluginDir, err)
	}
	want := filepath.Join(argValue(t, cmd, "--state-dir"), "plugins")
	if init := invocations()[0]; !init.hasArg("-plugin-dir=" + want) {
		t.Errorf("init args = %v, want -plugin-dir=%s", init.args, want)
	}
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Errorf("fallback plugin dir: %v", err)
	}
}

// setupValidate builds a `--command=validate` invocation matching the
// TofuValidate action. Every flag path is relative to the runner's working
// directory, including a bare filename for --tofu. TMPDIR points to a
// subdirectory of dir. The fake tofu prints a progress line on stdout and
// exits 3 on the subcommand named by FAKE_FAIL. It returns the command, its
// working directory, the stamp path and a reader for recorded tofu calls.
func setupValidate(t *testing.T) (cmd *exec.Cmd, dir, stamp string, invocations func() []invocation) {
	t.Helper()
	bin := runnerBin(t)
	// Resolve t.TempDir() so dir matches filepath.Abs and $(pwd); on macOS,
	// /var is a symlink to /private/var.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	logPath := filepath.Join(dir, "tofu-invocations.log")
	script := fakeTofuScript("if [ \"$1\" = show ]; then if [ -z \"${FAKE_SHOW-}\" ]; then FAKE_SHOW='{\"root_module\":{\"variables\":{\"name\":{}}}}'; fi; printf '%s' \"$FAKE_SHOW\"; exit 0; fi\necho \"progress of $1\"\nif [ \"$1\" = \"${FAKE_FAIL:-}\" ]; then echo \"$1 broke\" >&2; exit 3; fi\n")
	if err := os.WriteFile(filepath.Join(dir, "tofu.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	for f, content := range map[string]string{
		"sources/main.tf":          "# main\n",
		"sources/t.tftest.hcl":     "# test\n",
		"modules.json":             testManifest,
		"lock/.terraform.lock.hcl": "# lock\n",
	} {
		p := filepath.Join(dir, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o444); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "work", "mypkg", "tests"), 0o755); err != nil {
		t.Fatal(err)
	}
	for link, src := range map[string]string{"mypkg/main.tf": "main.tf", "mypkg/tests/t.tftest.hcl": "t.tftest.hcl"} {
		if err := os.Symlink(filepath.Join(dir, "sources", src), filepath.Join(dir, "work", filepath.FromSlash(link))); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	invocations = func() []invocation {
		t.Helper()
		data, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatalf("read tofu invocation log: %v", err)
		}
		return parseInvocations(data)
	}
	cmd = exec.Command(bin,
		"--command=validate",
		"--tofu=tofu.sh",
		"--work-tree=work",
		"--package-dir=mypkg",
		"--plugin-dir=work.plugins",
		"--modules-manifest=modules.json",
		"--stamp=validate.stamp",
	)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TOFU_LOG="+logPath, "TMPDIR="+filepath.Join(dir, "tmp"))
	return cmd, dir, filepath.Join(dir, "validate.stamp"), invocations
}

// `--command=validate` resolves relative flag paths against its working
// directory and mirrors the work tree as symlinks under TMPDIR. In the run
// directory there it runs `tofu init` with stdout discarded, `tofu validate`
// with stdout kept and `tofu show -json -module=.` to check --var names. It
// then writes --stamp, removes the temporary directory under TMPDIR and
// leaves the work tree untouched.
func TestRunner_Validate_InitsValidatesAndStamps(t *testing.T) {
	cmd, dir, stamp, invocations := setupValidate(t)
	cmd.Args = append(cmd.Args, "--lock-file=lock/.terraform.lock.hcl", "--test-dir=tests", "--var-file=sources/main.tf", "--var=name=a b", "--", "-json")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "progress of init") || !strings.Contains(string(out), "progress of validate") {
		t.Errorf("output = %q, want validate's stdout and not init's", out)
	}
	invs := invocations()
	if len(invs) != 3 {
		t.Fatalf("got %d tofu invocations, want init, validate and show: %+v", len(invs), invs)
	}
	init, validate, show := invs[0], invs[1], invs[2]
	if want := []string{"show", "-json", "-module=."}; !reflect.DeepEqual(show.args, want) || show.cwd != validate.cwd {
		t.Errorf("show ran %q in %q, want %q in %q", show.args, show.cwd, want, validate.cwd)
	}
	if init.cwd != validate.cwd || filepath.Base(init.cwd) != "mypkg" || strings.HasPrefix(init.cwd, filepath.Join(dir, "work")) {
		t.Errorf("cwd = %q and %q, want one staged mypkg outside the work tree", init.cwd, validate.cwd)
	}
	wantInit := []string{"init", "-backend=false", "-input=false", "-get=false", "-test-directory=tests"}
	for _, arg := range wantInit {
		if !init.hasArg(arg) {
			t.Errorf("init lacks %q: %v", arg, init.args)
		}
	}
	if !init.hasArgWithPrefix("-plugin-dir=/") || init.hasArg("-plugin-dir="+filepath.Join(dir, "work.plugins")) {
		t.Errorf("init args = %v, want an absolute fallback -plugin-dir", init.args)
	}
	varArgs := []string{"-var=name=a b", "-var-file=" + filepath.Join(dir, "sources", "main.tf")}
	if got := init.args[len(init.args)-len(varArgs):]; !reflect.DeepEqual(got, varArgs) {
		t.Errorf("init args end %q, want %q", got, varArgs)
	}
	wantValidate := append([]string{"validate", "-test-directory=tests"}, varArgs...)
	wantValidate = append(wantValidate, "-json")
	if !reflect.DeepEqual(validate.args, wantValidate) {
		t.Errorf("validate args = %q, want %q", validate.args, wantValidate)
	}
	if want := filepath.Join(init.cwd, ".terraform"); init.env["TF_DATA_DIR"] != want {
		t.Errorf("TF_DATA_DIR = %q, want %q", init.env["TF_DATA_DIR"], want)
	}
	if _, err := os.Stat(stamp); err != nil {
		t.Errorf("stamp not written: %v", err)
	}
	if left, err := os.ReadDir(filepath.Join(dir, "tmp")); err != nil || len(left) != 0 {
		t.Errorf("TMPDIR holds %v after the run (err=%v), want nothing", left, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "work", "mypkg", ".terraform")); !os.IsNotExist(err) {
		t.Errorf("the work tree gained .terraform: err=%v", err)
	}
}

// An existing --plugin-dir is resolved to an absolute path and used in place.
func TestRunner_Validate_PluginDirUsedInPlace(t *testing.T) {
	cmd, dir, _, invocations := setupValidate(t)
	pluginDir := filepath.Join(dir, "work.plugins")
	if err := os.MkdirAll(filepath.Join(pluginDir, "registry.opentofu.org"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	if init := invocations()[0]; !init.hasArg("-plugin-dir=" + pluginDir) {
		t.Errorf("init args = %v, want -plugin-dir=%s", init.args, pluginDir)
	}
}

// When a rule has no files, --work-tree does not exist and validation runs on
// an empty root module. `--test-dir=.` names the root module's own directory
// and passes no -test-directory flag.
func TestRunner_Validate_NoWorkTree(t *testing.T) {
	cmd, dir, stamp, invocations := setupValidate(t)
	if err := os.RemoveAll(filepath.Join(dir, "work")); err != nil {
		t.Fatal(err)
	}
	cmd.Args = append(cmd.Args, "--lock-file=lock/.terraform.lock.hcl", "--test-dir=.")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	for _, inv := range invocations() {
		if inv.hasArgWithPrefix("-test-directory") {
			t.Errorf("%v: -test-directory passed for the root module's own directory", inv.args)
		}
	}
	if _, err := os.Stat(stamp); err != nil {
		t.Errorf("stamp not written: %v", err)
	}
}

// When `tofu init` or `tofu validate` fails, the runner prints only the
// remediation hints that apply, writes no stamp, removes its scratch
// directory under TMPDIR and exits with tofu's status code.
func TestRunner_Validate_FailureAdvice(t *testing.T) {
	const (
		pin     = "bazel run @tf_providers//:pin"
		modules = "modules are never fetched"
		empty   = "so `tofu init` installs no providers"
		fetched = "fetched module @tf_modules//net failed validation"
	)
	withCalls := `{"Modules":[{"Key":"","Source":"","Dir":"."},{"Key":"net","Source":"./net","Dir":"net"}]}`
	for _, tc := range []struct {
		name      string
		fail      string
		flags     []string
		manifest  string
		emptyRoot bool
		want      []string
		notWant   []string
	}{
		{name: "init", fail: "init", want: []string{"init broke"}, notWant: []string{pin, modules, empty, fetched}},
		{name: "init with a lock", fail: "init", flags: []string{"--lock-file=lock/.terraform.lock.hcl"}, want: []string{pin}, notWant: []string{modules}},
		{name: "init with module calls", fail: "init", manifest: withCalls, want: []string{modules}, notWant: []string{pin}},
		{name: "init of a fetched module", fail: "init", flags: []string{"--fetched-module=@tf_modules//net"}, want: []string{fetched}},
		{name: "validate", fail: "validate", flags: []string{"--lock-file=lock/.terraform.lock.hcl"}, manifest: withCalls, want: []string{"validate broke"}, notWant: []string{pin, modules, empty, fetched}},
		{name: "validate of a fetched module", fail: "validate", flags: []string{"--fetched-module=@tf_modules//net"}, want: []string{fetched}},
		{name: "validate of a test of an empty root", fail: "validate", flags: []string{"--test-dir=."}, emptyRoot: true, want: []string{"the root module (mypkg) has no configuration files", empty}},
		{name: "validate of an empty root", fail: "validate", emptyRoot: true, notWant: []string{empty}},
		{name: "validate of a test", fail: "validate", flags: []string{"--test-dir=tests"}, notWant: []string{empty}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd, dir, stamp, _ := setupValidate(t)
			if tc.manifest != "" {
				if err := os.WriteFile(filepath.Join(dir, "manifest-with-calls.json"), []byte(tc.manifest), 0o644); err != nil {
					t.Fatal(err)
				}
				cmd.Args = append(cmd.Args, "--modules-manifest=manifest-with-calls.json")
			}
			if tc.emptyRoot {
				if err := os.Remove(filepath.Join(dir, "work", "mypkg", "main.tf")); err != nil {
					t.Fatal(err)
				}
			}
			cmd.Args = append(cmd.Args, tc.flags...)
			cmd.Env = append(cmd.Env, "FAKE_FAIL="+tc.fail)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 3 {
				t.Fatalf("err = %v, want exit code 3\n%s", err, out)
			}
			for _, w := range tc.want {
				if !strings.Contains(string(out), w) {
					t.Errorf("output lacks %q:\n%s", w, out)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(string(out), w) {
					t.Errorf("output has %q:\n%s", w, out)
				}
			}
			if _, err := os.Stat(stamp); !os.IsNotExist(err) {
				t.Errorf("stamp written despite the failure: err=%v", err)
			}
			if left, err := os.ReadDir(filepath.Join(dir, "tmp")); err != nil || len(left) != 0 {
				t.Errorf("TMPDIR holds %v after the run (err=%v), want nothing", left, err)
			}
		})
	}
}

// `--command=validate` requires --stamp.
func TestRunner_Validate_StampRequired(t *testing.T) {
	cmd, _, _, _ := setupValidate(t)
	for i, a := range cmd.Args {
		if strings.HasPrefix(a, "--stamp=") {
			cmd.Args = append(cmd.Args[:i], cmd.Args[i+1:]...)
			break
		}
	}
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "--stamp is required") {
		t.Errorf("err=%v out=%q, want --stamp is required", err, out)
	}
}

// A --var naming a variable that the root module does not declare fails
// validation and writes no stamp. `tofu validate` itself accepts it.
func TestRunner_Validate_UndeclaredVar(t *testing.T) {
	cmd, _, stamp, _ := setupValidate(t)
	cmd.Args = append(cmd.Args, "--var=name=a", "--var=region=b", "--var=zone=c")
	cmd.Env = append(cmd.Env, `FAKE_SHOW={"root_module":{"variables":{"name":{},"other":{}}}}`)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("runner passed with undeclared --var names:\n%s", out)
	}
	if want := "`vars` sets region, zone, which the root module does not declare as a variable"; !strings.Contains(string(out), want) {
		t.Errorf("output = %q, want it to contain %q", out, want)
	}
	if _, err := os.Stat(stamp); !os.IsNotExist(err) {
		t.Errorf("stamp written despite the failure: err=%v", err)
	}
}

// Without --var, validation does not ask tofu for the module's variables.
func TestRunner_Validate_NoVarsNoShow(t *testing.T) {
	cmd, _, _, invocations := setupValidate(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	for _, inv := range invocations() {
		if inv.hasArg("show") {
			t.Errorf("tofu show ran without --var: %v", inv.args)
		}
	}
}
