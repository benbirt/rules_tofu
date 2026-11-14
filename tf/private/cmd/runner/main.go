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
//   - --package-dir: workspace-relative directory of the root module within
//     --work-tree, or "" when the target is in the workspace root package.
//   - --lock-file: .terraform.lock.hcl from tf_providers.from_lock.
//   - --var-file: .tfvars or .tfvars.json file passed via -var-file,
//     repeatable.
//   - --test-dir: for --command=test, test directory relative to --package-dir,
//     passed as -test-directory; "" uses OpenTofu's default "tests".
//   - --allow-ephemeral-state: permit apply and destroy when state would live
//     under --output-root.
//
// Trailing arguments after flags are forwarded verbatim to `tofu`.
//
// For plan, apply and destroy, runner performs these steps in order:
//   - Refuse apply and destroy without --allow-ephemeral-state when the root
//     module declares no state backend or declares a `backend "local"` whose
//     path and workspace_dir are not both literal absolute paths outside
//     --output-root, which `bazel clean` deletes.
//   - Mirror --work-tree as symlinks into <state-dir>/work (see rundir.go). The
//     deploy's .plan, .apply and .destroy share that directory so OpenTofu
//     resolves every relative path itself and each launcher sees the same
//     files.
//   - Copy --modules-manifest and any --lock-file into
//     <state-dir>/work/<package-dir>, and run `tofu init -input=false
//     -get=false -reconfigure -plugin-dir=<plugin-dir>` there with TF_DATA_DIR
//     fixed to its `.terraform`.
//   - Without a backend or cloud block, pass -state and -state-out for
//     <state-dir>/terraform.tfstate; with one, pass no state flags so OpenTofu
//     owns state placement.
//   - For plan, write <state-dir>/tfplan via -out and print
//     `rules_tofu: plan saved to <path>` to stdout.
//   - For apply and destroy, run `tofu apply` or `tofu destroy`.
//
// For test, runner copies --work-tree into TEST_TMPDIR or a temporary
// directory, initialises with -backend=false and runs `tofu test` there (see
// test.go). When TEST_UNDECLARED_OUTPUTS_DIR is set, it writes JSON test output
// there and preserves any errored_test.tfstate left by a failed destroy.
//
// If `tofu` exits non-zero, runner exits with that code; other errors exit 1,
// or 2 on invalid flags. Configuration validation via TofuValidate on
// tf_library, tf_deploy and tf_test and duplicate-variable checks via dupcheck
// happen at `bazel build` time, not here.
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
	"github.com/benbirt/rules_tofu/tf/private/cmd/internal/tfconfig"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
)

var (
	workTree        = flag.String("work-tree", "", "path to the materialized work tree root")
	packageDir      = flag.String("package-dir", "", "workspace-relative dir of the root module within the work tree")
	pluginDir       = flag.String("plugin-dir", "", "absolute path to the vendored provider plugin tree, passed to tofu init via -plugin-dir")
	varFiles        = flags.StringList("var-file", "path to a .tfvars.json file passed to tofu via -var-file (repeatable)")
	modulesManifest = flag.String("modules-manifest", "", "the .terraform/modules/modules.json recording every module call, copied into place before tofu init -get=false")
	lockFile        = flag.String("lock-file", "", "optional .terraform.lock.hcl pinning the providers (tf_providers.from_lock), copied next to the configuration before tofu init")
	testDir         = flag.String("test-dir", "", "for --command=test, the test files' directory relative to the package dir, passed to tofu as -test-directory; empty for tofu's default")

	stateDir   = flag.String("state-dir", "", "absolute path to the per-deploy state directory; tofu runs in its work/ subdirectory")
	outputRoot = flag.String("output-root", "", "absolute path to Bazel's output tree (bazel-out); a local backend's state inside it is ephemeral")

	allowEphemeralState = flag.Bool("allow-ephemeral-state", false,
		"permit apply/destroy against local state under bazel-bin, for a deploy with no backend or with a local one that keeps state there; "+
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

	// Backend detection is a read-only scan of the root module's configuration
	// files in the launcher's work tree, so it depends neither on init having
	// run nor on the run directory existing. Compute it once, up front: the
	// gate below needs the answer before any tofu process is started or any
	// directory is created, and the state flags reuse it.
	backend := detectBackend(srcDir, *outputRoot)
	planFile := filepath.Join(*stateDir, "tfplan")
	stateFile := filepath.Join(*stateDir, "terraform.tfstate")

	// Refuse to mutate real infrastructure against throwaway local state.
	// Deliberately fails ahead of everything else: no tofu runs and the state
	// directory is not created. `plan` is ungated — planning against empty
	// local state destroys nothing.
	if (*command == "apply" || *command == "destroy") && !backend.durable() && !*allowEphemeralState {
		if backend.present {
			return localBackendError(*command, backend, cwd)
		}
		return ephemeralStateError(*command, stateFile)
	}

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
	// -reconfigure resets the backend settings saved in `.terraform/` from
	// earlier runs without migrating state; without it, init refuses an edited
	// backend block until `bazel clean`.
	if err := runTofu(env, cwd, "init", "-input=false", "-get=false", "-reconfigure", "-plugin-dir="+*pluginDir); err != nil {
		if *lockFile != "" {
			log.Print("if the provider versions in required_providers changed, run `bazel run @tf_providers//:pin` to update the lock file.")
		}
		return fmt.Errorf("tofu init: %w", err)
	}
	// With no backend block, state goes beside the run directory rather than
	// in it. With one, tofu owns where state lives: the runner passes no state
	// flags and never overrides the block, a `backend "local"` included, whose
	// relative paths tofu resolves against the run directory.
	stateArgs := []string{"-state=" + stateFile, "-state-out=" + stateFile}
	if backend.present {
		stateArgs = nil
	}
	varFileArgs := make([]string, len(*varFiles))
	for i, f := range *varFiles {
		varFileArgs[i] = "-var-file=" + f
	}

	switch *command {
	case "plan":
		args := append([]string{"plan", "-input=false", "-out=" + planFile}, varFileArgs...)
		args = append(args, stateArgs...)
		args = append(args, extraArgs...)
		if err := runTofu(env, cwd, args...); err != nil {
			return fmt.Errorf("tofu plan: %w", err)
		}
		fmt.Printf("rules_tofu: plan saved to %s\n", planFile)
	case "apply":
		applyArgs := append([]string{"apply", "-input=false"}, varFileArgs...)
		applyArgs = append(applyArgs, stateArgs...)
		applyArgs = append(applyArgs, extraArgs...)
		if err := runTofu(env, cwd, applyArgs...); err != nil {
			return fmt.Errorf("tofu apply: %w", err)
		}
	case "destroy":
		destroyArgs := append([]string{"destroy", "-input=false"}, varFileArgs...)
		destroyArgs = append(destroyArgs, stateArgs...)
		destroyArgs = append(destroyArgs, extraArgs...)
		if err := runTofu(env, cwd, destroyArgs...); err != nil {
			return fmt.Errorf("tofu destroy: %w", err)
		}
	}
	return nil
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

// ephemeralExplanation says why state under bazel-bin is disposable; each
// refusal supplies the sentence's subject, and optInAdvice explains how to opt
// in.
const (
	ephemeralExplanation = `is machine-local and disposable. It sits under bazel-bin, so
"bazel clean" deletes it; it embeds the build configuration, so building with
-c opt or a different --platforms silently forks to a different state file; and
a colleague running this target from their own checkout gets their own private
copy. Losing or forking that file orphans every resource this command creates:
the infrastructure carries on existing with nothing tracking it.`

	optInAdvice = `If disposable state is the point — a demo or example, a throwaway sandbox, or
bootstrapping the bucket that will later hold real state — opt in explicitly by
setting

    allow_ephemeral_state = True

on the tf_deploy target. For the bootstrap case, add the remote backend block
once the bucket exists and move the existing state into it with:

    tofu init -migrate-state

The deploy's .plan target is never gated, so planning still works either way.`
)

// ephemeralStateError is the refusal returned when apply/destroy would write
// local state for a deploy with no backend. It is the whole user-facing
// surface of the gate, so it names the state path, the recommended fix and the
// opt-in verbatim.
func ephemeralStateError(command, stateFile string) error {
	return fmt.Errorf(`rules_tofu: refusing to run "tofu %s": this deploy declares no state backend, so its state would be written to

    %s

That path %s

Recommended fix — declare a remote backend in a .tf file listed in this
deploy's srcs:

    terraform {
      backend "gcs" {
        bucket = "my-tofu-state"
        prefix = "prod"
      }
    }

(Any remote backend or a cloud {} block will do. A backend "local" block
counts only when its path and workspace_dir are both literal absolute paths
outside Bazel's output tree, and that state is still local to this machine.)
With one present, the runner defers to it and this check no longer applies.

%s`,
		command, stateFile, ephemeralExplanation, optInAdvice)
}

// localBackendError is the refusal returned when apply/destroy would run
// against a `backend "local"` block that doesn't provably keep its state
// outside Bazel's output tree. It names the block and the attribute that
// fails the test, where tofu runs, the fix and the opt-in.
func localBackendError(command string, backend backendInfo, cwd string) error {
	return fmt.Errorf(`rules_tofu: refusing to run "tofu %s": the backend "local" block at %s does not provably keep state outside Bazel's output tree: %s.

tofu resolves that block itself, from this deploy's run directory,

    %s

which is where a relative path, or the default of an attribute left unset,
leads. State there %s

To keep this state, set both path and workspace_dir to literal absolute paths
outside Bazel's output tree:

    terraform {
      backend "local" {
        path          = "/var/lib/tofu/prod/terraform.tfstate"
        workspace_dir = "/var/lib/tofu/prod/workspaces"
      }
    }

or declare a remote backend (s3, gcs, ...) or a cloud {} block.

%s`,
		command, backend.where, backend.ephemeral, cwd, ephemeralExplanation, optInAdvice)
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

// terraformBlockSchema matches top-level `terraform {}` blocks;
// backendBlockSchema matches the `backend "type" {}` and `cloud {}` blocks
// nested within one. Both are used with schema-based PartialContent, which
// decodes native .tf and .tofu and JSON .tf.json and .tofu.json syntax
// uniformly and ignores every other construct.
var (
	terraformBlockSchema = &hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{{Type: "terraform"}},
	}
	backendBlockSchema = &hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			{Type: "backend", LabelNames: []string{"type"}},
			{Type: "cloud"},
		},
	}
	// The attributes of a `backend "local"` block that name a path: `path`
	// holds the default workspace's state, `workspace_dir` the others'.
	localBackendSchema = &hcl.BodySchema{
		Attributes: []hcl.AttributeSchema{{Name: "path"}, {Name: "workspace_dir"}},
	}
)

// backendInfo is what the root module's configuration says about where state
// lives.
type backendInfo struct {
	// present is whether a `backend "..." {}` or `cloud {}` block was found;
	// tofu then owns where state lives and the runner passes no -state flags.
	present bool
	// where names the block, as `<file>:<line>` relative to the package dir.
	where string
	// ephemeral is set for a `backend "local"` that doesn't provably keep its
	// state outside Bazel's output tree (see localBackendEphemeral). It says
	// which attribute fails the test and why.
	ephemeral string
}

// durable reports whether state outlives `bazel clean`: a backend is declared,
// and it isn't a local one that may keep state in Bazel's output tree.
// Anything else needs --allow-ephemeral-state for apply and destroy.
func (b backendInfo) durable() bool {
	return b.present && b.ephemeral == ""
}

// detectBackend scans the effective configuration files in the deploy's
// package directory dir (see tfconfig.ConfigFiles) for the first
// `backend "..." {}` or `cloud {}` block inside a top-level `terraform {}`
// block. Either construct means the configuration chooses its own state
// backend and the local -state and -state-out flags are omitted. A
// `backend "local"` block is also checked against Bazel's output tree
// outputRoot (see localBackendEphemeral).
//
// A file that cannot be read or parsed is skipped: build-time validation
// already checks syntax, so a parse error here must not crash the runner.
//
// Only dir is scanned, never the full work tree. OpenTofu reads backend
// configuration from the root module only, where `tofu init` runs; transitive
// tf_library deps materialised elsewhere in the work tree are not the root
// module, so backend blocks in their files have no effect.
func detectBackend(dir, outputRoot string) backendInfo {
	files, err := tfconfig.ConfigFiles(dir)
	if err != nil {
		return backendInfo{}
	}
	parser := hclparse.NewParser()
	for _, path := range files {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		name := filepath.Base(path)
		var file *hcl.File
		var diags hcl.Diagnostics
		if strings.HasSuffix(name, ".json") {
			file, diags = parser.ParseJSON(b, name)
		} else {
			file, diags = parser.ParseHCL(b, name)
		}
		if diags.HasErrors() {
			continue
		}
		if info, ok := backendIn(file, name, outputRoot); ok {
			return info
		}
	}
	return backendInfo{}
}

// backendIn returns the first backend or cloud block of file, if any.
func backendIn(file *hcl.File, name, outputRoot string) (backendInfo, bool) {
	content, _, diags := file.Body.PartialContent(terraformBlockSchema)
	if diags.HasErrors() {
		return backendInfo{}, false
	}
	for _, block := range content.Blocks {
		inner, _, diags := block.Body.PartialContent(backendBlockSchema)
		if diags.HasErrors() {
			continue
		}
		for _, b := range inner.Blocks {
			info := backendInfo{present: true, where: fmt.Sprintf("%s:%d", name, b.DefRange.Start.Line)}
			if b.Type == "backend" && b.Labels[0] == "local" {
				info.ephemeral = localBackendEphemeral(b.Body, outputRoot)
			}
			return info, true
		}
	}
	return backendInfo{}, false
}

// localBackendAttrs are the attributes of a `backend "local"` block that name
// a path, with the defaults tofu gives them: `path` holds the default
// workspace's state and `workspace_dir` the other workspaces'.
var localBackendAttrs = []struct{ name, def string }{
	{"path", "terraform.tfstate"},
	{"workspace_dir", "terraform.tfstate.d"},
}

// localBackendEphemeral decides whether a `backend "local"` block provably
// keeps its state outside Bazel's output tree outputRoot, returning why not,
// or "" when it does. The runner never changes what the block means: tofu
// resolves it from the run directory, which is under bazel-bin, so anything
// relative lands there and goes with `bazel clean`.
//
// State is provably outside only when both `path` and `workspace_dir` are set
// to literal absolute paths that do not lead into outputRoot once symlinks are
// followed, such as a checkout's bazel-bin convenience link. An unset
// attribute takes tofu's relative default; a non-literal expression, which
// OpenTofu 1.8 allows, cannot be evaluated statically here.
func localBackendEphemeral(body hcl.Body, outputRoot string) string {
	content, _, diags := body.PartialContent(localBackendSchema)
	if diags.HasErrors() {
		return "its configuration could not be read"
	}
	root, err := evalSymlinksAllowMissing(outputRoot)
	if err != nil {
		return fmt.Sprintf("Bazel's output tree (%s) could not be resolved: %v", outputRoot, err)
	}
	for _, attr := range localBackendAttrs {
		a, ok := content.Attributes[attr.name]
		if !ok {
			return fmt.Sprintf("its `%s` is not set, and tofu's default for it (%q) is relative", attr.name, attr.def)
		}
		v, ok := tfconfig.LiteralString(a.Expr)
		if !ok {
			return fmt.Sprintf("its `%s` is not a literal string, so the runner can't tell where it leads", attr.name)
		}
		if !filepath.IsAbs(v) {
			return fmt.Sprintf("its `%s` (%q) is relative", attr.name, v)
		}
		resolved, err := evalSymlinksAllowMissing(v)
		if err != nil {
			return fmt.Sprintf("its `%s` (%q) could not be resolved: %v", attr.name, v, err)
		}
		if within(root, resolved) {
			return fmt.Sprintf("its `%s` (%q) is inside Bazel's output tree (%s)", attr.name, v, root)
		}
	}
	return ""
}

// evalSymlinksAllowMissing is filepath.EvalSymlinks for a path whose tail may
// not exist yet, as a state file doesn't before the first apply: the longest
// existing ancestor is resolved and the missing components re-joined to it.
func evalSymlinksAllowMissing(p string) (string, error) {
	p = filepath.Clean(p)
	var tail []string
	for {
		resolved, err := filepath.EvalSymlinks(p)
		if err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", err
		}
		tail = append(tail, filepath.Base(p))
		p = parent
	}
}

// within reports whether path is dir or something under it. Both are clean
// and absolute.
func within(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
