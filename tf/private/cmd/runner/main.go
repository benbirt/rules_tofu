// Command runner is rules_tofu's tofu plan/apply/test wrapper. It is invoked by
// the generated shell launcher emitted by the tf_runner rule with every
// configuration value passed via explicit standard flags.
//
// Responsibilities:
//   - Validate we were invoked via `bazel run` (BUILD_WORKSPACE_DIRECTORY set).
//   - Refuse apply/destroy when the deploy declares no state backend and has
//     not opted in via --allow-ephemeral-state, since local state under
//     bazel-bin does not survive `bazel clean`. Resolve a `backend "local"`'s
//     relative paths against the configuration's directory in the checkout,
//     as tofu would from that directory, rather than against the runfiles
//     work tree tofu runs in here; refuse every command when that isn't
//     possible (an expression, or a deploy from another repository).
//   - Run `tofu init -get=false -reconfigure` inside the pre-materialized
//     work tree, with TF_DATA_DIR fixed to its `.terraform`, after copying in
//     the module manifest (every module is already in the work tree) and the
//     provider lock when providers come from tf_providers.from_lock.
//   - For plan: emit a plan artifact and stop.
//   - For apply/destroy: delegate to tofu, passing through any extra args.
//   - For test (tf_test): copy the work tree into $TEST_TMPDIR, init it with
//     -backend=false and run `tofu test` there (see test.go).
//
// Configuration validation (`tofu init -backend=false && tofu validate`)
// happens at `bazel build` time via the deploy rule's TofuValidate action,
// not here. Duplicate-variable-key detection across `vars` and `var_files`
// likewise happens at build time via the dupcheck action.
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
	packageDir      = flag.String("package-dir", "", "workspace-relative dir to cd into within the work tree")
	pluginDir       = flag.String("plugin-dir", "", "absolute path to the vendored provider plugin tree, passed to tofu init via -plugin-dir")
	varFiles        = flags.StringList("var-file", "path to a .tfvars.json file passed to tofu via -var-file (repeatable)")
	modulesManifest = flag.String("modules-manifest", "", "the .terraform/modules/modules.json recording every module call, copied into place before tofu init -get=false")
	lockFile        = flag.String("lock-file", "", "optional .terraform.lock.hcl pinning the providers (tf_providers.from_lock), copied next to the configuration before tofu init")
	testDir         = flag.String("test-dir", "", "for --command=test, the test files' directory relative to the package dir, passed to tofu as -test-directory; empty for tofu's default")

	stateDir = flag.String("state-dir", "", "absolute path to the per-deploy state directory")

	allowEphemeralState = flag.Bool("allow-ephemeral-state", false,
		"permit apply/destroy against local state under bazel-bin when the deploy declares no backend; "+
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
	// itself is the cwd.
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

	if os.Getenv("BUILD_WORKSPACE_DIRECTORY") == "" {
		return errors.New(
			"rules_tofu runner must be invoked via `bazel run`. " +
				"BUILD_WORKSPACE_DIRECTORY is unset, so state cannot be persisted.",
		)
	}
	cwd := filepath.Join(*workTree, *packageDir)
	if _, err := os.Stat(cwd); err != nil {
		return fmt.Errorf("work-tree cwd %s: %w", cwd, err)
	}

	// Backend detection is a read-only scan of the root module's .tf files, so
	// it does not depend on init having run. Compute it once, up front: the
	// gates below need the answer before any tofu process is started or any
	// directory is created, and the state flags reuse it. A `backend "local"`
	// gets its paths resolved against the configuration's directory in the
	// checkout (see localBackendConfig), which a deploy from another
	// repository doesn't have.
	srcPkgDir := filepath.Join(os.Getenv("BUILD_WORKSPACE_DIRECTORY"), filepath.FromSlash(*packageDir))
	external := *packageDir == "external" || strings.HasPrefix(*packageDir, "external/")
	backend := detectBackend(cwd, srcPkgDir, external)
	planFile := filepath.Join(*stateDir, "tfplan")
	stateFile := filepath.Join(*stateDir, "terraform.tfstate")

	// A `backend "local"` the runner can't resolve can never mean what it
	// says here: tofu would resolve it against the launcher's runfiles work
	// tree, a per-target directory Bazel owns. Refuse every command, plan
	// included, since a plan against a state file in the wrong place misleads
	// rather than destroys. Ahead of the ephemeral-state gate, so the opt-in
	// doesn't let it through either.
	if backend.problem != "" {
		return localBackendError(*command, backend)
	}

	// Refuse to mutate real infrastructure against throwaway local state.
	// Deliberately fails ahead of everything else: no tofu runs and the state
	// directory is not created. `plan` is ungated — planning against empty
	// local state destroys nothing.
	if (*command == "apply" || *command == "destroy") && !backend.present && !*allowEphemeralState {
		return ephemeralStateError(*command, stateFile)
	}

	if err := os.MkdirAll(*stateDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", *stateDir, err)
	}

	env := tofuEnv(cwd)

	// Ensure the vendored plugin tree exists even when zero providers are in
	// scope (the deploy declares no symlinks under the sibling <name>.plugins/
	// tree in that case, so the runfiles tree lacks the directory).
	// -plugin-dir overrides all default plugin search paths and prevents the
	// registry from being contacted at runtime.
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
	// -reconfigure: the runfiles work tree keeps the `.terraform/` of earlier
	// runs, whose saved backend configuration would otherwise make init refuse
	// an edited backend block (the only way out being `bazel clean`). It
	// resets that saved copy from the configuration and never migrates state.
	initArgs := append([]string{"init", "-input=false", "-get=false", "-reconfigure", "-plugin-dir=" + *pluginDir}, backend.initArgs...)
	if err := runTofu(env, cwd, initArgs...); err != nil {
		if *lockFile != "" {
			log.Print("if the provider versions in required_providers changed, run `bazel run @tf_providers//:pin` to update the lock file.")
		}
		return fmt.Errorf("tofu init: %w", err)
	}
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

// installLockFile copies the provider lock to cwd/.terraform.lock.hcl, so
// init checks the configuration's constraints and the vendored packages
// against it. Whatever is there already (a lock tofu wrote on an earlier run
// in this runfiles tree) is replaced, never written through.
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
// earlier run left in this runfiles tree is replaced, never written through.
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

// ephemeralStateError is the refusal returned when apply/destroy would write
// local state. It is the whole user-facing surface of the gate, so it names the
// state path, the recommended fix, and the opt-in verbatim.
func ephemeralStateError(command, stateFile string) error {
	return fmt.Errorf(`rules_tofu: refusing to run "tofu %s": this deploy declares no state backend, so its state would be written to

    %s

That path is machine-local and disposable. It sits under bazel-bin, so
"bazel clean" deletes it; it embeds the build configuration, so building with
-c opt or a different --platforms silently forks to a different state file; and
a colleague running this target from their own checkout gets their own private
copy. Losing or forking that file orphans every resource this command creates:
the infrastructure carries on existing with nothing tracking it.

Recommended fix — declare a remote backend in a .tf file listed in this
deploy's srcs:

    terraform {
      backend "gcs" {
        bucket = "my-tofu-state"
        prefix = "prod"
      }
    }

(Any remote backend or a cloud {} block will do. A backend "local" block
counts too, its paths resolved against the configuration's directory in your
checkout, but that state is as machine-local as this one.) With one present,
the runner defers to it and this check no longer applies.

If disposable state is the point — a demo or example, a throwaway sandbox, or
bootstrapping the bucket that will later hold real state — opt in explicitly by
setting

    allow_ephemeral_state = True

on the tf_deploy target. For the bootstrap case, add the backend block once the
bucket exists and move the existing state into it with:

    tofu init -migrate-state

The deploy's .plan target is never gated, so planning still works either way.`,
		command, stateFile)
}

// localBackendError is the refusal for a `backend "local"` block whose paths
// the runner can't resolve. It names the block, says why, and lists the
// configurations that do work.
func localBackendError(command string, backend backendInfo) error {
	return fmt.Errorf(`rules_tofu: refusing to run "tofu %s": the backend "local" block at %s can't be used: %s.

The runner resolves a local backend's relative path and workspace_dir (and
their defaults, terraform.tfstate and terraform.tfstate.d) against the
configuration's directory in this checkout, as "tofu -chdir" would, because
tofu's working directory here is the launcher's runfiles work tree: a
per-target directory that Bazel owns and prunes, where a relative path would
mean a different, disposable state file for each of .plan, .apply and .destroy.
What it can't resolve it refuses: an expression, which tofu would evaluate
against that runfiles tree, or a relative path for a deploy in another
repository, which has no directory in this checkout.

Use one of:

  - a remote backend (s3, gcs, ...) or a cloud {} block;
  - backend "local" with literal paths, absolute where the runner can't
    resolve them;
  - no backend block, with allow_ephemeral_state = True on the tf_deploy, for
    state under bazel-bin that is disposable by design.`,
		command, backend.where, backend.problem)
}

// tofuEnv is the environment tofu runs with: the user's, less the variables
// that would move tofu's data directory or plugin cache away from the work
// tree, plus TF_IN_AUTOMATION and a TF_DATA_DIR fixed to the work tree's own
// `.terraform`, where the module manifest and the lock are installed. A user's
// TF_DATA_DIR would otherwise make `init -get=false` miss the manifest at run
// time only (the validate action runs with a clean environment), and a plugin
// cache would be consulted and written to beside the vendored plugin tree.
//
// TF_IN_AUTOMATION=1 suppresses usage-hint lines in tofu output that don't
// apply in our wrapped invocation (e.g. "Run `terraform plan`…"). TF_INPUT is
// deliberately not set: variable prompts are suppressed per-command via
// -input=false, but the apply/destroy approval prompt must remain reachable.
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

// terraformBlockSchema matches top-level `terraform {}` blocks; backendBlockSchema
// matches the `backend "type" {}` and `cloud {}` blocks nested within one. Both
// are used with schema-based PartialContent, which decodes native (.tf, .tofu)
// and JSON (.tf.json, .tofu.json) syntax uniformly and ignores every other
// construct.
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
	// the runner then defers to it and passes no -state flags.
	present bool
	// where names the block, as `<file>:<line>` relative to the package dir.
	where string
	// initArgs are the `-backend-config` arguments for `tofu init` that pin a
	// `backend "local"`'s paths to their resolved, absolute values (see
	// localBackendConfig). Empty for any other backend.
	initArgs []string
	// problem is set for a `backend "local"` the runner can't resolve: a
	// path that is an expression, or a relative one for a deploy in another
	// repository. It says what is wrong.
	problem string
}

// detectBackend scans the configuration files tofu reads in the deploy's
// package directory (tfconfig.ConfigFiles: a `.tofu` file shadows the `.tf`
// file of the same name) for a `backend "..." {}` or `cloud {}` block nested
// inside a top-level `terraform {}` block, and returns what it finds. Either
// construct means the configuration chooses its own state backend, and the
// local -state/-state-out flags are omitted. A `backend "local"`'s paths are
// resolved against srcPkgDir, the configuration's directory in the checkout
// (see localBackendConfig); external says the deploy is from another
// repository, which has none.
//
// A file that fails to parse is treated as backend-less. The build-time validate
// action already guarantees well-formed files at runtime, so a parse error here
// must not crash the runner.
//
// Only the package directory (cwd) is scanned — not the full work tree. This is
// intentional: Terraform reads backend configuration from the root module only,
// which is the directory where `tofu init` runs. Transitive library deps are
// materialised at their own workspace-relative paths (e.g. sibling directories)
// and are not the root module, so backend blocks in those files do not affect
// the root module's state backend.
func detectBackend(cwd, srcPkgDir string, external bool) backendInfo {
	files, err := tfconfig.ConfigFiles(cwd)
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
		if info, ok := backendIn(file, name, srcPkgDir, external); ok {
			return info
		}
	}
	return backendInfo{}
}

// backendIn returns the first backend or cloud block of file, if any.
func backendIn(file *hcl.File, name, srcPkgDir string, external bool) (backendInfo, bool) {
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
				info.initArgs, info.problem = localBackendConfig(b.Body, srcPkgDir, external)
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

// localBackendConfig resolves a `backend "local"` block's paths as
// `tofu -chdir=<pkg>` would outside Bazel: a relative `path` or
// `workspace_dir`, or the default of one left unset, is taken relative to the
// configuration's directory in the checkout, srcPkgDir. Here tofu's working
// directory is the runfiles work tree instead, so the resolved absolute paths
// go to `tofu init` as -backend-config arguments, which override the block's
// (and, with -reconfigure, are re-applied on every run).
//
// What can't be resolved is a problem, returned as the second value: an
// attribute that is an expression (OpenTofu 1.8 allows them; `path.module`
// would name the runfiles tree, and a variable can't be read here), or a
// relative path when external is set, since a deploy from another repository
// has no directory in the checkout.
func localBackendConfig(body hcl.Body, srcPkgDir string, external bool) (args []string, problem string) {
	content, _, diags := body.PartialContent(localBackendSchema)
	if diags.HasErrors() {
		return nil, "its configuration could not be read"
	}
	for _, attr := range localBackendAttrs {
		v := attr.def
		if a, ok := content.Attributes[attr.name]; ok {
			s, ok := tfconfig.LiteralString(a.Expr)
			if !ok {
				return nil, fmt.Sprintf("its `%s` is not a literal string, so the runner can't tell where it leads", attr.name)
			}
			v = s
		}
		if !filepath.IsAbs(v) {
			if external {
				return nil, fmt.Sprintf("its `%s` (%q) is relative and the deploy is in another repository, which has no directory in this checkout to resolve it against; set it to an absolute path", attr.name, v)
			}
			v = filepath.Join(srcPkgDir, filepath.FromSlash(v))
		}
		args = append(args, fmt.Sprintf("-backend-config=%s=%s", attr.name, v))
	}
	return args, ""
}
