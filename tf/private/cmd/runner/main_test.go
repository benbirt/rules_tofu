package main_test

import (
	"encoding/json"
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

// fakeTofuScript is the stand-in for tofu: it logs its working directory,
// arguments and the environment variables the runner is meant to control,
// then runs `extra` (shell) and exits 0.
func fakeTofuScript(extra string) string {
	return "#!/bin/sh\n" +
		"{ printf 'cwd=%s\\n' \"$(pwd)\"; for a in \"$@\"; do printf 'arg=%s\\n' \"$a\"; done; " +
		"printf 'env=TF_DATA_DIR=%s\\n' \"${TF_DATA_DIR-<unset>}\"; " +
		"printf 'env=TF_PLUGIN_CACHE_DIR=%s\\n' \"${TF_PLUGIN_CACHE_DIR-<unset>}\"; " +
		"printf -- '---\\n'; } >> \"$TOFU_LOG\"\n" +
		extra +
		"exit 0\n"
}

// setup creates a temporary work tree with the given vars written to
// rules_tofu.auto.tfvars.json, and a fake tofu script that logs each invocation
// to a temp file. It returns:
//   - a pre-configured runner Cmd (callers may append --var-file flags before running)
//   - addVarFile: writes a .tfvars.json file and returns its path
//   - invocations: reads and returns all recorded tofu invocations
func setup(t *testing.T, vars map[string]any) (
	cmd *exec.Cmd,
	addVarFile func(map[string]any) string,
	invocations func() []invocation,
) {
	t.Helper()
	return setupPkg(t, vars, "mypkg")
}

// setupPkg is like setup but places the deploy at the given workspace-relative
// package directory. An empty pkgDir models a deploy in the workspace root
// package, where the work tree root itself holds the root module.
//
// The runner is given a state directory `<tmp>/state`, so tofu runs in
// `<tmp>/state/work/<pkgDir>` (see runDir), and an output root `<tmp>/out`.
func setupPkg(t *testing.T, vars map[string]any, pkgDir string) (
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
	writeJSON(t, filepath.Join(workTree, pkgDir, "rules_tofu.auto.tfvars.json"), vars)

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
	cmd, _, invocations := setup(t, map[string]any{"region": "us-east-1"})
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	invs := invocations()
	// Expect: init, plan.
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
	cmd, addVarFile, invocations := setup(t, map[string]any{"region": "us-east-1"})
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

// TestRunner_WithDataFile verifies that a work tree containing arbitrary data
// files (e.g. placed there by the `data` attribute) does not interfere with
// normal runner execution. The files are simply present on disk so that
// Terraform's file() function can read them.
func TestRunner_WithDataFile(t *testing.T) {
	cmd, _, invocations := setup(t, map[string]any{"region": "us-east-1"})

	// Simulate a data file materialized by the `data` attribute — its location
	// is determined at analysis time by the Bazel rule (short_path under the
	// work tree), so here we just place it inside the work tree directory.
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
	cmd, _, invocations := setup(t, map[string]any{"region": "us-east-1"})
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
	cmd, _, invocations := setup(t, map[string]any{"region": "us-east-1"})
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
	// Expect: init, apply.
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
	cmd, _, invocations := setup(t, map[string]any{"region": "us-east-1"})
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
	// Expect: init, destroy.
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

// TestRunner_StateFlags_Apply verifies that the apply invocation receives
// -state= and -state-out= flags when no backend block is present. The
// ephemeral-state opt-in is required to reach apply at all in that shape.
func TestRunner_StateFlags_Apply(t *testing.T) {
	cmd, _, invocations := setup(t, map[string]any{"region": "us-east-1"})
	cmd.Args = append(cmd.Args, "--command=apply", "--allow-ephemeral-state")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	invs := invocations()
	// Expect: init, apply.
	if len(invs) != 2 {
		t.Fatalf("expected 2 tofu invocations (init, apply), got %d: %+v", len(invs), invs)
	}
	apply := invs[1]
	if !apply.hasArg("apply") {
		t.Errorf("second invocation should be 'apply', got args: %v", apply.args)
	}
	if !apply.hasArgWithPrefix("-state=") {
		t.Errorf("apply invocation missing -state=, got args: %v", apply.args)
	}
	if !apply.hasArgWithPrefix("-state-out=") {
		t.Errorf("apply invocation missing -state-out=, got args: %v", apply.args)
	}
}

// TestRunner_StateFlags_Destroy verifies that the destroy invocation receives
// -state= and -state-out= flags when no backend block is present. The
// ephemeral-state opt-in is required to reach destroy at all in that shape.
func TestRunner_StateFlags_Destroy(t *testing.T) {
	cmd, _, invocations := setup(t, map[string]any{"region": "us-east-1"})
	cmd.Args = append(cmd.Args, "--command=destroy", "--allow-ephemeral-state")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	invs := invocations()
	// Expect: init, destroy.
	if len(invs) != 2 {
		t.Fatalf("expected 2 tofu invocations (init, destroy), got %d: %+v", len(invs), invs)
	}
	destroy := invs[1]
	if !destroy.hasArg("destroy") {
		t.Errorf("second invocation should be 'destroy', got args: %v", destroy.args)
	}
	if !destroy.hasArgWithPrefix("-state=") {
		t.Errorf("destroy invocation missing -state=, got args: %v", destroy.args)
	}
	if !destroy.hasArgWithPrefix("-state-out=") {
		t.Errorf("destroy invocation missing -state-out=, got args: %v", destroy.args)
	}
}

// backendTFContent is a helper that writes a .tf file with a terraform block
// containing the given inner content into the package directory of the work
// tree, and returns the path written.
func backendTFContent(t *testing.T, cmd *exec.Cmd, content string) {
	t.Helper()
	tfPath := filepath.Join(argValue(t, cmd, "--work-tree"), argValue(t, cmd, "--package-dir"), "backend_override.tf")
	if err := os.WriteFile(tfPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// backendJSONContent is backendTFContent for a `.tf.json` file.
func backendJSONContent(t *testing.T, cmd *exec.Cmd, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(argValue(t, cmd, "--work-tree"), argValue(t, cmd, "--package-dir"), "backend_override.tf.json"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRunner_BackendBlock_OmitsStateFlags verifies that when a terraform{}
// backend "..." {} block is present, the runner does not pass -state= or
// -state-out= to tofu.
func TestRunner_BackendBlock_OmitsStateFlags(t *testing.T) {
	cmd, _, invocations := setup(t, map[string]any{"region": "us-east-1"})
	backendTFContent(t, cmd, `
terraform {
  backend "s3" {}
}
`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	invs := invocations()
	if len(invs) != 2 {
		t.Fatalf("expected 2 tofu invocations (init, plan), got %d: %+v", len(invs), invs)
	}
	plan := invs[1]
	if plan.hasArgWithPrefix("-state=") {
		t.Errorf("plan invocation should not have -state= when backend block present, got args: %v", plan.args)
	}
	if plan.hasArgWithPrefix("-state-out=") {
		t.Errorf("plan invocation should not have -state-out= when backend block present, got args: %v", plan.args)
	}
}

// TestRunner_CloudBlock_OmitsStateFlags verifies that when a terraform{}
// cloud {} block is present (the modern HCP Terraform alternative to backend),
// the runner does not pass -state= or -state-out= to tofu.
func TestRunner_CloudBlock_OmitsStateFlags(t *testing.T) {
	cmd, _, invocations := setup(t, map[string]any{"region": "us-east-1"})
	backendTFContent(t, cmd, `
terraform {
  cloud {}
}
`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	invs := invocations()
	if len(invs) != 2 {
		t.Fatalf("expected 2 tofu invocations (init, plan), got %d: %+v", len(invs), invs)
	}
	plan := invs[1]
	if plan.hasArgWithPrefix("-state=") {
		t.Errorf("plan invocation should not have -state= when cloud block present, got args: %v", plan.args)
	}
	if plan.hasArgWithPrefix("-state-out=") {
		t.Errorf("plan invocation should not have -state-out= when cloud block present, got args: %v", plan.args)
	}
}

// writePkgFile writes a file with the given basename and content into the
// package directory of the work tree.
func writePkgFile(t *testing.T, cmd *exec.Cmd, name, content string) {
	t.Helper()
	var workTree string
	for _, arg := range cmd.Args {
		if strings.HasPrefix(arg, "--work-tree=") {
			workTree = strings.TrimPrefix(arg, "--work-tree=")
		}
	}
	if workTree == "" {
		t.Fatal("could not find --work-tree flag in runner cmd args")
	}
	path := filepath.Join(workTree, "mypkg", name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRunner_BackendBlock_JSON_OmitsStateFlags verifies that a backend block
// declared in Terraform JSON syntax (.tf.json) is recognised, so the runner
// omits -state= / -state-out= just as it does for native .tf syntax.
func TestRunner_BackendBlock_JSON_OmitsStateFlags(t *testing.T) {
	cmd, _, invocations := setup(t, map[string]any{"region": "us-east-1"})
	writePkgFile(t, cmd, "backend.tf.json", `{"terraform": {"backend": {"s3": {}}}}`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	invs := invocations()
	if len(invs) != 2 {
		t.Fatalf("expected 2 tofu invocations (init, plan), got %d: %+v", len(invs), invs)
	}
	plan := invs[1]
	if plan.hasArgWithPrefix("-state=") {
		t.Errorf("plan invocation should not have -state= when .tf.json backend block present, got args: %v", plan.args)
	}
	if plan.hasArgWithPrefix("-state-out=") {
		t.Errorf("plan invocation should not have -state-out= when .tf.json backend block present, got args: %v", plan.args)
	}
}

// TestRunner_BackendBlock_Tofu_OmitsStateFlags verifies that backend blocks in
// OpenTofu-only files (.tofu, .tofu.json) are recognised.
func TestRunner_BackendBlock_Tofu_OmitsStateFlags(t *testing.T) {
	for name, content := range map[string]string{
		"backend.tofu":      "terraform {\n  backend \"s3\" {}\n}\n",
		"backend.tofu.json": `{"terraform": {"backend": {"s3": {}}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			cmd, _, invocations := setup(t, map[string]any{"region": "us-east-1"})
			writePkgFile(t, cmd, name, content)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
			}
			invs := invocations()
			if len(invs) != 2 {
				t.Fatalf("expected 2 tofu invocations (init, plan), got %d: %+v", len(invs), invs)
			}
			if plan := invs[1]; plan.hasArgWithPrefix("-state=") || plan.hasArgWithPrefix("-state-out=") {
				t.Errorf("plan invocation should not have local state flags when %s declares a backend, got args: %v", name, plan.args)
			}
		})
	}
}

// TestRunner_BackendBlock_ShadowedByTofu_KeepsStateFlags verifies that a backend
// block in a .tf file doesn't count when a .tofu file of the same name, which
// tofu reads instead, declares none.
func TestRunner_BackendBlock_ShadowedByTofu_KeepsStateFlags(t *testing.T) {
	cmd, _, invocations := setup(t, map[string]any{"region": "us-east-1"})
	writePkgFile(t, cmd, "backend.tf", "terraform {\n  backend \"s3\" {}\n}\n")
	writePkgFile(t, cmd, "backend.tofu", `terraform { required_version = ">= 1.8" }`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	invs := invocations()
	if len(invs) != 2 {
		t.Fatalf("expected 2 tofu invocations (init, plan), got %d: %+v", len(invs), invs)
	}
	if plan := invs[1]; !plan.hasArgWithPrefix("-state=") || !plan.hasArgWithPrefix("-state-out=") {
		t.Errorf("plan invocation should have local state flags when backend.tofu shadows backend.tf, got args: %v", plan.args)
	}
}

// TestRunner_NoBackendBlock_JSON_KeepsStateFlags verifies that a .tf.json file
// with a terraform block but no backend/cloud block does not suppress the local
// -state= / -state-out= flags.
func TestRunner_NoBackendBlock_JSON_KeepsStateFlags(t *testing.T) {
	cmd, _, invocations := setup(t, map[string]any{"region": "us-east-1"})
	writePkgFile(t, cmd, "main.tf.json", `{"terraform": {"required_version": ">= 1.0"}}`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	invs := invocations()
	if len(invs) != 2 {
		t.Fatalf("expected 2 tofu invocations (init, plan), got %d: %+v", len(invs), invs)
	}
	plan := invs[1]
	if !plan.hasArgWithPrefix("-state=") {
		t.Errorf("plan invocation should have -state= when .tf.json has no backend block, got args: %v", plan.args)
	}
	if !plan.hasArgWithPrefix("-state-out=") {
		t.Errorf("plan invocation should have -state-out= when .tf.json has no backend block, got args: %v", plan.args)
	}
}

// TestRunner_MalformedJSON_KeepsStateFlags verifies that an unparseable .tf.json
// file is treated as backend-less: the runner keeps the local -state= /
// -state-out= flags and does not crash. Build-time validate guarantees
// well-formed files at runtime, so this is a defensive backstop only.
func TestRunner_MalformedJSON_KeepsStateFlags(t *testing.T) {
	cmd, _, invocations := setup(t, map[string]any{"region": "us-east-1"})
	writePkgFile(t, cmd, "broken.tf.json", `{"terraform": {`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly with malformed .tf.json present: %v\n%s", err, out)
	}
	invs := invocations()
	if len(invs) != 2 {
		t.Fatalf("expected 2 tofu invocations (init, plan), got %d: %+v", len(invs), invs)
	}
	plan := invs[1]
	if !plan.hasArgWithPrefix("-state=") {
		t.Errorf("plan invocation should have -state= when .tf.json is malformed, got args: %v", plan.args)
	}
	if !plan.hasArgWithPrefix("-state-out=") {
		t.Errorf("plan invocation should have -state-out= when .tf.json is malformed, got args: %v", plan.args)
	}
}

// TestRunner_NoCheckoutNeeded verifies that the runner is an ordinary
// executable: it runs with no BUILD_WORKSPACE_DIRECTORY (setup removes it, as
// when a built launcher is run directly rather than through `bazel run`) and
// from an unrelated working directory, and tofu still runs in the deploy's
// run directory.
func TestRunner_NoCheckoutNeeded(t *testing.T) {
	cmd, _, invocations := setup(t, map[string]any{"region": "us-east-1"})
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
			cmd, _, _ := setup(t, map[string]any{"region": "us-east-1"})
			// Remove the flag from Args, keeping all others.
			filtered := cmd.Args[:1] // keep argv[0] (binary path)
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

// TestRunner_BackendInSiblingDir verifies that a backend block located in a
// sibling directory of the package dir (simulating a transitive tf_library dep
// materialised at its own workspace-relative path) does NOT suppress the
// -state= / -state-out= flags.
//
// Terraform reads backend configuration only from the root module — the
// directory where `tofu init` is invoked (packageDir). Files from library deps
// land at their own workspace-relative paths and are child modules, not the
// root module, so their backend blocks are irrelevant to the root module's
// state backend. hasBackend intentionally scans only the package directory.
func TestRunner_BackendInSiblingDir(t *testing.T) {
	cmd, _, invocations := setup(t, map[string]any{"region": "us-east-1"})

	// Find the work tree root from the runner args.
	var workTree string
	for _, arg := range cmd.Args {
		if strings.HasPrefix(arg, "--work-tree=") {
			workTree = strings.TrimPrefix(arg, "--work-tree=")
		}
	}
	if workTree == "" {
		t.Fatal("could not find --work-tree flag in runner cmd args")
	}

	// Simulate a tf_library dep materialised at a sibling path (e.g.
	// "somelib/" next to "mypkg/"). Place a backend block there — it should
	// be invisible to hasBackend, which only scans the package dir.
	siblingDir := filepath.Join(workTree, "somelib")
	if err := os.MkdirAll(siblingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	siblingBackend := filepath.Join(siblingDir, "backend.tf")
	if err := os.WriteFile(siblingBackend, []byte(`
terraform {
  backend "s3" {}
}
`), 0o644); err != nil {
		t.Fatal(err)
	}

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	invs := invocations()
	if len(invs) != 2 {
		t.Fatalf("expected 2 tofu invocations (init, plan), got %d: %+v", len(invs), invs)
	}
	plan := invs[1]
	// Backend block is in a sibling dir, not in the package dir — the runner
	// must still pass -state= and -state-out= for the root module.
	if !plan.hasArgWithPrefix("-state=") {
		t.Errorf("plan invocation should have -state= when backend is only in sibling dir, got args: %v", plan.args)
	}
	if !plan.hasArgWithPrefix("-state-out=") {
		t.Errorf("plan invocation should have -state-out= when backend is only in sibling dir, got args: %v", plan.args)
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

// assertRefusedEphemeralState asserts that the runner refused a mutating
// command: it exited non-zero, named the opt-in attribute, ran no tofu process
// at all, and did not create the state directory.
func assertRefusedEphemeralState(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	logPath := envValue(t, cmd, "TOFU_LOG")
	stateDir := argValue(t, cmd, "--state-dir")

	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected runner to refuse local state, but it succeeded:\n%s", out)
	}
	msg := string(out)
	if !strings.Contains(msg, "allow_ephemeral_state") {
		t.Errorf("expected refusal to name allow_ephemeral_state, got: %s", msg)
	}
	if !strings.Contains(msg, stateDir) {
		t.Errorf("expected refusal to name the state path %s, got: %s", stateDir, msg)
	}

	// The gate must fail before any tofu process starts, so the fake tofu's
	// log must be absent (or, defensively, empty). invocations() cannot be used
	// here: it t.Fatal's on a missing log file.
	if info, err := os.Stat(logPath); err == nil {
		if info.Size() > 0 {
			data, readErr := os.ReadFile(logPath)
			if readErr != nil {
				t.Fatalf("read tofu invocation log: %v", readErr)
			}
			t.Errorf("expected no tofu invocations, got: %+v", parseInvocations(data))
		}
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat tofu invocation log: %v", err)
	}

	if _, err := os.Stat(stateDir); err == nil {
		t.Errorf("state dir %s should not have been created", stateDir)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat state dir: %v", err)
	}
}

// TestRunner_EphemeralState_ApplyRefused verifies that apply against a deploy
// with no backend block and no opt-in is refused outright: no tofu runs and the
// state directory is not created.
func TestRunner_EphemeralState_ApplyRefused(t *testing.T) {
	cmd, _, _ := setup(t, map[string]any{"region": "us-east-1"})
	cmd.Args = append(cmd.Args, "--command=apply")
	assertRefusedEphemeralState(t, cmd)
}

// TestRunner_EphemeralState_DestroyRefused verifies that destroy is gated the
// same way as apply.
func TestRunner_EphemeralState_DestroyRefused(t *testing.T) {
	cmd, _, _ := setup(t, map[string]any{"region": "us-east-1"})
	cmd.Args = append(cmd.Args, "--command=destroy")
	assertRefusedEphemeralState(t, cmd)
}

// TestRunner_EphemeralState_PlanUngated verifies that plan is deliberately not
// gated: planning against empty local state destroys nothing, so it runs and
// still receives the local -state= / -state-out= flags.
func TestRunner_EphemeralState_PlanUngated(t *testing.T) {
	cmd, _, invocations := setup(t, map[string]any{"region": "us-east-1"})
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("plan should not be gated, but the runner failed: %v\n%s", err, out)
	}
	invs := invocations()
	if len(invs) != 2 {
		t.Fatalf("expected 2 tofu invocations (init, plan), got %d: %+v", len(invs), invs)
	}
	plan := invs[1]
	if !plan.hasArg("plan") {
		t.Errorf("second invocation should be 'plan', got args: %v", plan.args)
	}
	if !plan.hasArgWithPrefix("-state=") {
		t.Errorf("plan invocation missing -state=, got args: %v", plan.args)
	}
	if !plan.hasArgWithPrefix("-state-out=") {
		t.Errorf("plan invocation missing -state-out=, got args: %v", plan.args)
	}
}

// TestRunner_EphemeralState_ApplyAllowed verifies that --allow-ephemeral-state
// restores today's behaviour: apply runs and keeps the local state flags.
func TestRunner_EphemeralState_ApplyAllowed(t *testing.T) {
	cmd, _, invocations := setup(t, map[string]any{"region": "us-east-1"})
	cmd.Args = append(cmd.Args, "--command=apply", "--allow-ephemeral-state")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly with --allow-ephemeral-state: %v\n%s", err, out)
	}
	invs := invocations()
	// Expect: init, apply.
	if len(invs) != 2 {
		t.Fatalf("expected 2 tofu invocations (init, apply), got %d: %+v", len(invs), invs)
	}
	if !invs[0].hasArg("init") {
		t.Errorf("first invocation should be 'init', got args: %v", invs[0].args)
	}
	apply := invs[1]
	if !apply.hasArg("apply") {
		t.Errorf("second invocation should be 'apply', got args: %v", apply.args)
	}
	if !apply.hasArgWithPrefix("-state=") {
		t.Errorf("apply invocation missing -state=, got args: %v", apply.args)
	}
	if !apply.hasArgWithPrefix("-state-out=") {
		t.Errorf("apply invocation missing -state-out=, got args: %v", apply.args)
	}
}

// TestRunner_EphemeralState_DestroyAllowed verifies the opt-in applies to
// destroy as well as apply.
func TestRunner_EphemeralState_DestroyAllowed(t *testing.T) {
	cmd, _, invocations := setup(t, map[string]any{"region": "us-east-1"})
	cmd.Args = append(cmd.Args, "--command=destroy", "--allow-ephemeral-state")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly with --allow-ephemeral-state: %v\n%s", err, out)
	}
	invs := invocations()
	// Expect: init, destroy.
	if len(invs) != 2 {
		t.Fatalf("expected 2 tofu invocations (init, destroy), got %d: %+v", len(invs), invs)
	}
	destroy := invs[1]
	if !destroy.hasArg("destroy") {
		t.Errorf("second invocation should be 'destroy', got args: %v", destroy.args)
	}
	if !destroy.hasArgWithPrefix("-state=") {
		t.Errorf("destroy invocation missing -state=, got args: %v", destroy.args)
	}
	if !destroy.hasArgWithPrefix("-state-out=") {
		t.Errorf("destroy invocation missing -state-out=, got args: %v", destroy.args)
	}
}

// TestRunner_EphemeralState_BackendApplyAllowed verifies that the gate keys off
// backend detection rather than the opt-in flag: with a backend block present,
// apply runs without the flag and the local state flags stay omitted.
func TestRunner_EphemeralState_BackendApplyAllowed(t *testing.T) {
	cmd, _, invocations := setup(t, map[string]any{"region": "us-east-1"})
	backendTFContent(t, cmd, `
terraform {
  backend "s3" {}
}
`)
	cmd.Args = append(cmd.Args, "--command=apply")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly with a backend block present: %v\n%s", err, out)
	}
	invs := invocations()
	// Expect: init, apply.
	if len(invs) != 2 {
		t.Fatalf("expected 2 tofu invocations (init, apply), got %d: %+v", len(invs), invs)
	}
	apply := invs[1]
	if !apply.hasArg("apply") {
		t.Errorf("second invocation should be 'apply', got args: %v", apply.args)
	}
	if apply.hasArgWithPrefix("-state=") {
		t.Errorf("apply invocation should not have -state= when backend block present, got args: %v", apply.args)
	}
	if apply.hasArgWithPrefix("-state-out=") {
		t.Errorf("apply invocation should not have -state-out= when backend block present, got args: %v", apply.args)
	}
}

// TestRunner_EmptyPackageDir_RunsAtRunDirRoot verifies that an empty
// --package-dir is valid: it models a deploy in the workspace root package, so
// tofu runs with the run directory's root itself as its cwd. (The BCR test
// module e2e/smoke has exactly this shape.)
func TestRunner_EmptyPackageDir_RunsAtRunDirRoot(t *testing.T) {
	cmd, _, invocations := setupPkg(t, map[string]any{"region": "us-east-1"}, "")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly with empty --package-dir: %v\n%s", err, out)
	}
	invs := invocations()
	// Expect: init, plan.
	if len(invs) != 2 {
		t.Fatalf("expected 2 tofu invocations (init, plan), got %d: %+v", len(invs), invs)
	}
	if !invs[0].hasArg("init") {
		t.Errorf("first invocation should be 'init', got args: %v", invs[0].args)
	}
	if !invs[1].hasArg("plan") {
		t.Errorf("second invocation should be 'plan', got args: %v", invs[1].args)
	}
	// The recorded cwd must be the run directory's root itself, not a package
	// subdirectory.
	for _, inv := range invs {
		assertSameDir(t, inv.cwd, runDir(t, cmd))
	}
	assertLink(t, filepath.Join(runDir(t, cmd), "rules_tofu.auto.tfvars.json"), filepath.Join(argValue(t, cmd, "--work-tree"), "rules_tofu.auto.tfvars.json"))
}

// TestRunner_LockFile_CopiedBeforeInit verifies that --lock-file lands at
// <cwd>/.terraform.lock.hcl in the run directory, replacing a lock left by an
// earlier run, so init checks against the pinned lock.
func TestRunner_LockFile_CopiedBeforeInit(t *testing.T) {
	cmd, _, invocations := setup(t, map[string]any{})
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
	cmd, _, _ := setup(t, map[string]any{})
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

// setupTest builds a `--command=test` invocation, as the launcher a tf_test
// generates makes it: no --state-dir and no --output-root, a
// TEST_TMPDIR to copy the work tree into, and a TEST_UNDECLARED_OUTPUTS_DIR.
// The fake tofu logs its calls and, when FAKE_ERRORED is set, leaves an
// errored_test.tfstate behind on `test`. It returns the command, the work
// tree in runfiles, the scratch directory and the outputs directory.
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
	writeJSON(t, filepath.Join(workTree, "mypkg", "rules_tofu.auto.tfvars.json"), map[string]any{"name": "x"})

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
		"--var-file=mypkg/test.tfvars",
	)
	cmd.Env = append(os.Environ(),
		"TOFU_LOG="+logPath,
		"TEST_TMPDIR="+scratch,
		"TEST_UNDECLARED_OUTPUTS_DIR="+outputs,
	)
	return cmd, workTree, scratch, outputs, invocations
}

// `--command=test` copies the work tree into TEST_TMPDIR, inits it offline
// with -backend=false and the test directory, and runs `tofu test` there with
// the JSON log in the outputs, the var files re-based onto the copy and the
// extra arguments last. The runfiles work tree is left as it was.
func TestRunner_Test_InitsAndTestsACopy(t *testing.T) {
	cmd, workTree, scratch, outputs, invocations := setupTest(t)
	cmd.Args = append(cmd.Args, "--", "-verbose")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	invs := invocations()
	if len(invs) != 2 {
		t.Fatalf("got %d tofu invocations, want init then test: %+v", len(invs), invs)
	}
	copyPkg := filepath.Join(scratch, "rules_tofu.work", "mypkg")
	// The fake tofu logs $(pwd), a resolved path: on macOS /var is a symlink
	// to /private/var.
	wantCwd, err := filepath.EvalSymlinks(copyPkg)
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
	wantTest := []string{"test", "-no-color", "-test-directory=tests", "-json-into=" + filepath.Join(outputs, "tofu-test.json"),
		"-var-file=" + filepath.Join(scratch, "rules_tofu.work", "mypkg", "test.tfvars"), "-verbose"}
	if !reflect.DeepEqual(test.args, wantTest) {
		t.Errorf("test args = %q, want %q", test.args, wantTest)
	}
	// The copy holds the sources' contents, the manifest and the generated
	// tfvars; the runfiles tree gained nothing.
	if data, err := os.ReadFile(filepath.Join(copyPkg, "tests", "t.tftest.hcl")); err != nil || string(data) != "# mypkg/tests/t.tftest.hcl\n" {
		t.Errorf("copied test file = %q, %v", data, err)
	}
	for _, f := range []string{".terraform/modules/modules.json", "rules_tofu.auto.tfvars.json"} {
		if _, err := os.Stat(filepath.Join(copyPkg, f)); err != nil {
			t.Errorf("copy lacks %s: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(workTree, "mypkg", ".terraform")); !os.IsNotExist(err) {
		t.Errorf("runfiles work tree gained .terraform: err=%v", err)
	}
}

// With no providers in scope the plugin tree is absent from runfiles: tofu
// gets an empty directory under TEST_TMPDIR, and nothing is created in the
// runfiles tree.
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
	if want := filepath.Join(scratch, "rules_tofu.plugins"); got != want {
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

// `--command=test` needs no --state-dir; the other commands still do.
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

// init runs with -reconfigure in plan, apply and destroy, so an edited backend
// block isn't refused on the strength of the `.terraform/` an earlier run left
// in the run directory.
func TestRunner_Init_Reconfigure(t *testing.T) {
	cmd, _, invocations := setup(t, map[string]any{"region": "us-east-1"})
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runner failed unexpectedly: %v\n%s", err, out)
	}
	if init := invocations()[0]; !init.hasArg("init") || !init.hasArg("-reconfigure") {
		t.Errorf("init args = %v, want -reconfigure", init.args)
	}
}

// tofu runs with TF_DATA_DIR fixed to the run directory's `.terraform` and no
// TF_PLUGIN_CACHE_DIR, whatever the parent environment holds.
func TestRunner_Env_DataDirAndNoPluginCache(t *testing.T) {
	cmd, _, invocations := setup(t, map[string]any{"region": "us-east-1"})
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

// The same holds in test mode, where init keeps -backend=false and gains no
// -reconfigure, and TF_DATA_DIR is the copy's `.terraform`.
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
	want := filepath.Join(scratch, "rules_tofu.work", "mypkg", ".terraform")
	for _, inv := range invs {
		if inv.env["TF_DATA_DIR"] != want {
			t.Errorf("%s: TF_DATA_DIR = %q, want %q", inv.args[0], inv.env["TF_DATA_DIR"], want)
		}
		if inv.env["TF_PLUGIN_CACHE_DIR"] != "<unset>" {
			t.Errorf("%s: TF_PLUGIN_CACHE_DIR = %q, want it unset", inv.args[0], inv.env["TF_PLUGIN_CACHE_DIR"])
		}
	}
}

// runDir returns the deploy's run directory, `<state-dir>/work`: the mirror
// of the work tree that tofu runs in.
func runDir(t *testing.T, cmd *exec.Cmd) string {
	t.Helper()
	return filepath.Join(argValue(t, cmd, "--state-dir"), "work")
}

// setFakeTofu replaces the fake tofu with one that also runs `extra` (shell,
// in tofu's working directory, with the subcommand as $1).
func setFakeTofu(t *testing.T, cmd *exec.Cmd, extra string) {
	t.Helper()
	if err := os.WriteFile(argValue(t, cmd, "--tofu"), []byte(fakeTofuScript(extra)), 0o755); err != nil {
		t.Fatal(err)
	}
}

// withCommand returns a fresh copy of cmd (an exec.Cmd runs once) with its
// --command replaced and the given extra flags added.
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
// root, creating its directory.
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
	cmd, _, invocations := setup(t, map[string]any{"region": "us-east-1"})
	// The lock and the data directory are what tofu writes beside the
	// configuration.
	setFakeTofu(t, cmd, ": > .terraform.lock.hcl; mkdir -p .terraform/providers\n")
	files := []string{
		"mypkg/rules_tofu.auto.tfvars.json",
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
	cmd, addVarFile, invocations := setup(t, map[string]any{"region": "us-east-1"})
	base := filepath.Dir(argValue(t, cmd, "--work-tree"))
	// The runner makes them absolute against its working directory as the
	// kernel names it, which is not always how t.TempDir() spells it (on
	// macOS /var is a link to /private/var).
	physBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Args = append(cmd.Args, "--var-file="+addVarFile(map[string]any{"env": "prod"}))
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
		filepath.Join(want["--state-dir"], "work", "mypkg", "rules_tofu.auto.tfvars.json"),
		filepath.Join(want["--work-tree"], "mypkg", "rules_tofu.auto.tfvars.json"))
}

// The run directory follows the work tree from one run to the next: a file
// that left the work tree loses its link, a new one gains a link, and what
// tofu wrote there (here the state of a `backend "local"` with a relative
// path, issue #86) is kept, so the command after sees it.
func TestRunner_RunDir_FollowsWorkTreeAndKeepsState(t *testing.T) {
	cmd, _, _ := setup(t, map[string]any{"region": "us-east-1"})
	backendTFContent(t, cmd, "terraform {\n  backend \"local\" {\n    path = \"state/dev.tfstate\"\n  }\n}\n")
	// apply writes the state where tofu would for that block; destroy fails
	// unless it finds it.
	setFakeTofu(t, cmd, `case "$1" in
  apply) mkdir -p state && echo applied > state/dev.tfstate ;;
  destroy) [ "$(cat state/dev.tfstate)" = applied ] || { echo "no state" >&2; exit 1; } ;;
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
	if got, err := os.ReadFile(filepath.Join(cwd, "state", "dev.tfstate")); err != nil || string(got) != "applied\n" {
		t.Errorf("state in the run directory = %q, %v; want it kept", got, err)
	}
	assertWorkTreeUntouched(t, cmd)
}

// A regular file where a link must go is never overwritten: it is tofu's or
// the user's, and the runner stops before running tofu. An empty directory
// holds nothing and gives way.
func TestRunner_RunDir_FileInTheWay(t *testing.T) {
	cmd, _, _ := setup(t, map[string]any{"region": "us-east-1"})
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

// Of the `.terraform/` an earlier run left, `providers` goes (its links lead
// into that launcher's plugin tree, and init relinks them) and the rest
// stays: the workspace selection in `environment` is the user's.
func TestRunner_RunDir_DataDirAcrossRuns(t *testing.T) {
	cmd, _, _ := setup(t, map[string]any{"region": "us-east-1"})
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

// assertRefusedLocalBackend asserts that the runner refused a mutating
// command because of a `backend "local"` block that doesn't provably keep
// state outside Bazel's output tree: it exited non-zero, named the block, the
// reason, the run directory and the opt-in, ran no tofu process and created
// no state dir.
func assertRefusedLocalBackend(t *testing.T, cmd *exec.Cmd, block, reason string) {
	t.Helper()
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected the runner to refuse the local backend, but it succeeded:\n%s", out)
	}
	msg := string(out)
	for _, want := range []string{
		`backend "local" block at ` + block,
		reason,
		filepath.Join(runDir(t, cmd), "mypkg"),
		"allow_ephemeral_state = True",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected the refusal to contain %q, got: %s", want, msg)
		}
	}
	if _, err := os.Stat(envValue(t, cmd, "TOFU_LOG")); !os.IsNotExist(err) {
		t.Errorf("expected no tofu invocations, got err=%v", err)
	}
	stateDir := argValue(t, cmd, "--state-dir")
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Errorf("state dir %s should not have been created, got err=%v", stateDir, err)
	}
}

// assertLocalBackendLeftToTofu asserts that the last two invocations are init
// and the given command, and that neither overrides the backend block: no
// -backend-config for init, no -state flags for the command.
func assertLocalBackendLeftToTofu(t *testing.T, invs []invocation, command string) {
	t.Helper()
	if len(invs) < 2 {
		t.Fatalf("expected init and %s, got %+v", command, invs)
	}
	init, run := invs[len(invs)-2], invs[len(invs)-1]
	if !init.hasArg("init") || !run.hasArg(command) {
		t.Fatalf("expected init then %s, got %v then %v", command, init.args, run.args)
	}
	if init.hasArgWithPrefix("-backend-config") {
		t.Errorf("init args = %v, want no -backend-config: the block is tofu's to resolve", init.args)
	}
	if run.hasArgWithPrefix("-state=") || run.hasArgWithPrefix("-state-out=") {
		t.Errorf("%s args = %v, want no -state flags with a backend block present", command, run.args)
	}
}

// One rule decides whether a `backend "local"` needs allow_ephemeral_state for
// apply and destroy: its state is durable only when `path` and `workspace_dir`
// are both literal absolute paths that don't lead into Bazel's output tree.
// Everything else (an attribute left to its relative default, a relative
// path, an expression, a path into the output tree, directly or through a
// symlink, existing yet or not) is ephemeral: refused without the opt-in,
// naming the block and the attribute, and run with it. plan always runs. The
// runner never overrides the block, in HCL and JSON alike.
func TestRunner_LocalBackend_Gate(t *testing.T) {
	type file struct {
		json    bool
		content string
	}
	hclBlock := func(attrs string) file {
		return file{false, "terraform {\n  backend \"local\" {\n" + attrs + "  }\n}\n"}
	}
	cases := []struct {
		name string
		// config builds the backend file from a directory outside the output
		// tree, the output tree, and a symlink that leads into it.
		config func(outside, outputRoot, link string) file
		// reason is why the state is ephemeral; empty when it is durable.
		reason string
	}{
		{"both absolute", func(o, _, _ string) file {
			return hclBlock(fmt.Sprintf("    path          = %q\n    workspace_dir = %q\n", o+"/prod.tfstate", o+"/workspaces"))
		}, ""},
		{"both absolute, json", func(o, _, _ string) file {
			return file{true, fmt.Sprintf(`{"terraform": {"backend": {"local": {"path": %q, "workspace_dir": %q}}}}`, o+"/prod.tfstate", o+"/workspaces")}
		}, ""},
		{"empty block", func(_, _, _ string) file { return hclBlock("") },
			"its `path` is not set, and tofu's default for it (\"terraform.tfstate\") is relative"},
		{"relative path", func(_, _, _ string) file { return hclBlock("    path = \"state/dev.tfstate\"\n") },
			"its `path` (\"state/dev.tfstate\") is relative"},
		{"workspace_dir unset", func(o, _, _ string) file {
			return hclBlock(fmt.Sprintf("    path = %q\n", o+"/prod.tfstate"))
		}, "its `workspace_dir` is not set, and tofu's default for it (\"terraform.tfstate.d\") is relative"},
		{"relative workspace_dir", func(o, _, _ string) file {
			return hclBlock(fmt.Sprintf("    path          = %q\n    workspace_dir = \"ws\"\n", o+"/prod.tfstate"))
		}, "its `workspace_dir` (\"ws\") is relative"},
		{"expression", func(o, _, _ string) file {
			return hclBlock(fmt.Sprintf("    path          = \"${path.module}/dev.tfstate\"\n    workspace_dir = %q\n", o+"/workspaces"))
		}, "its `path` is not a literal string"},
		{"expression, json", func(o, _, _ string) file {
			return file{true, fmt.Sprintf(`{"terraform": {"backend": {"local": {"path": "${path.module}/dev.tfstate", "workspace_dir": %q}}}}`, o+"/workspaces")}
		}, "its `path` is not a literal string"},
		{"inside the output tree", func(o, root, _ string) file {
			return hclBlock(fmt.Sprintf("    path          = %q\n    workspace_dir = %q\n", root+"/cfg/bin/pkg/prod.tfstate", o+"/workspaces"))
		}, "is inside Bazel's output tree"},
		{"workspace_dir inside the output tree", func(o, root, _ string) file {
			return hclBlock(fmt.Sprintf("    path          = %q\n    workspace_dir = %q\n", o+"/prod.tfstate", root+"/ws"))
		}, "its `workspace_dir` (\"" + "%ROOT%" + "/ws\") is inside Bazel's output tree"},
		// As through the `bazel-bin` link in a checkout, to a state file and a
		// directory that don't exist yet.
		{"through a symlink into the output tree", func(o, _, link string) file {
			return hclBlock(fmt.Sprintf("    path          = %q\n    workspace_dir = %q\n", link+"/not/yet/prod.tfstate", o+"/workspaces"))
		}, "is inside Bazel's output tree"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd, _, invocations := setup(t, map[string]any{"region": "us-east-1"})
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
			f := tc.config(outside, outputRoot, link)
			block := "backend_override.tf:2"
			if f.json {
				backendJSONContent(t, cmd, f.content)
				block = "backend_override.tf.json:1"
			} else {
				backendTFContent(t, cmd, f.content)
			}
			reason := strings.ReplaceAll(tc.reason, "%ROOT%", outputRoot)

			if tc.reason != "" {
				for _, command := range []string{"apply", "destroy"} {
					assertRefusedLocalBackend(t, withCommand(cmd, command), block, reason)
				}
			}
			// plan is never gated; apply and destroy run without the opt-in
			// when the state is durable, and with it otherwise.
			var flags []string
			if tc.reason != "" {
				flags = []string{"--allow-ephemeral-state"}
			}
			if out, err := withCommand(cmd, "plan").CombinedOutput(); err != nil {
				t.Fatalf("plan failed unexpectedly: %v\n%s", err, out)
			}
			assertLocalBackendLeftToTofu(t, invocations(), "plan")
			for _, command := range []string{"apply", "destroy"} {
				if out, err := withCommand(cmd, command, flags...).CombinedOutput(); err != nil {
					t.Fatalf("%s failed unexpectedly: %v\n%s", command, err, out)
				}
				assertLocalBackendLeftToTofu(t, invocations(), command)
			}
		})
	}
}
