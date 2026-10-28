// Command pin is `bazel run @tf_providers//:pin`: it rewrites the workspace's
// .terraform.lock.hcl (tf_providers.from_lock) with `tofu providers lock`.
//
// The lock serves every tf_library and tf_deploy in the workspace, so it is
// locked against all of their version constraints:
//
//   - `bazel query` lists the .tf/.tf.json source files of every tf_library
//     and tf_deploy in the main repository (with their dependencies). Bazel
//     already knows which packages exist, so .bazelignore, deleted packages
//     and nested workspaces need no handling here. A .tf file outside every
//     rule, a versions.tf included, is not locked against. Modules from
//     other repositories (tf_modules, or a library with module_source) count
//     too, but only in the directories their library serves.
//   - Each file's `terraform { required_providers { ... } }` entries are
//     extracted, and nothing else, so resources, variables and module calls
//     that only make sense in place never need to load.
//   - Providers the root module pins with tf_providers.provider tags
//     (--exclude-source) are left out: the lock mustn't pin them too.
//   - A scratch configuration is generated with one child module per
//     directory, so tofu combines the constraints exactly as it would for
//     nested modules: a versions.tf's exact pins decide the versions, and
//     without one tofu picks the newest version every directory accepts.
//   - `tofu get` + `tofu providers lock` run there, from scratch: `providers
//     lock` keeps an existing lock's versions and has no -upgrade. The new
//     lock is copied over the old one.
//
// When the constraints can't all be met, tofu's error doesn't say where they
// came from, so pin lists them by provider and directory.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/benbirt/rules_tofu/tf/private/cmd/internal/flags"
	"github.com/benbirt/rules_tofu/tf/private/cmd/internal/tfconfig"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
)

var (
	tofu      = flag.String("tofu", "", "path to the tofu binary")
	lockDir   = flag.String("lock-dir", "", "workspace-relative directory of the .terraform.lock.hcl")
	platforms = flags.StringList("platform", "`<os>_<arch>` to lock for unless -platform= arguments follow (repeatable)")
	excluded  = flags.StringList("exclude-source", "`<namespace>/<name>` pinned by a tag instead, left out of the lock (repeatable)")
)

func main() {
	flag.Parse()
	workspace := os.Getenv("BUILD_WORKSPACE_DIRECTORY")
	if workspace == "" {
		fmt.Fprintln(os.Stderr, "rules_tofu: pin must be invoked via `bazel run @tf_providers//:pin`")
		os.Exit(1)
	}
	if err := run(workspace, *lockDir, *tofu, bazelCommand(), *platforms, *excluded, flag.Args(), os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "rules_tofu:", err)
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.ExitCode())
		}
		os.Exit(1)
	}
}

// requirement is one `required_providers` entry.
type requirement struct {
	Source  string `json:"source"`
	Version string `json:"version,omitempty"`
}

// module is the requirements found in one directory.
type module struct {
	Dir  string // workspace-relative ("" for the workspace root), or `@<repo>//<dir>`
	Reqs map[string]requirement
}

// bazelCommand is the Bazel to query with: $BAZEL, else bazel or bazelisk
// from PATH.
func bazelCommand() string {
	if b := os.Getenv("BAZEL"); b != "" {
		return b
	}
	for _, name := range []string{"bazel", "bazelisk"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	return "bazel"
}

func run(workspace, lockDir, tofu, bazel string, defaultPlatforms, excludedSources, extraArgs []string, log io.Writer) error {
	byDir, err := queryConfigFiles(bazel, workspace, log)
	if err != nil {
		return err
	}
	dirs := make([]string, 0, len(byDir))
	for dir := range byDir {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)

	var modules []module
	for _, dir := range dirs {
		reqs, err := parseFiles(byDir[dir])
		if err != nil {
			return err
		}
		reqs = withoutSources(reqs, excludedSources)
		if len(reqs) > 0 {
			modules = append(modules, module{Dir: dir, Reqs: reqs})
		}
	}
	if len(modules) == 0 {
		return errors.New("no tf_library or tf_deploy in the workspace declares required_providers to lock")
	}

	scratch, err := os.MkdirTemp("", "rules_tofu-pin-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	if err := writeScratch(scratch, modules); err != nil {
		return err
	}

	args := []string{"-chdir=" + scratch, "providers", "lock"}
	if !hasPlatformArg(extraArgs) {
		for _, p := range defaultPlatforms {
			args = append(args, "-platform="+p)
		}
	}
	args = append(args, extraArgs...)

	if err := runTofu(tofu, io.Discard, log, "-chdir="+scratch, "get"); err != nil {
		return fmt.Errorf("tofu get: %w", err)
	}
	if err := runTofu(tofu, log, log, args...); err != nil {
		fmt.Fprint(log, constraintsSummary(modules))
		return fmt.Errorf("tofu providers lock: %w", err)
	}

	lock, err := os.ReadFile(filepath.Join(scratch, ".terraform.lock.hcl"))
	if err != nil {
		return err
	}
	dst := filepath.Join(workspace, lockDir, ".terraform.lock.hcl")
	if err := os.WriteFile(dst, lock, 0o644); err != nil {
		return err
	}
	n := len(modules)
	fmt.Fprintf(log, "rules_tofu: wrote %s from the required_providers of %d director%s\n",
		dst, n, map[bool]string{true: "y", false: "ies"}[n == 1])
	return nil
}

// withoutSources drops the requirements on the given `<namespace>/<name>`
// sources. Sources compare as tofu does: case-insensitively, with the default
// registry host optional.
func withoutSources(reqs map[string]requirement, sources []string) map[string]requirement {
	drop := map[string]bool{}
	for _, s := range sources {
		drop[strings.ToLower(s)] = true
	}
	kept := map[string]requirement{}
	for name, req := range reqs {
		source := strings.TrimPrefix(strings.ToLower(req.Source), "registry.opentofu.org/")
		if !drop[source] {
			kept[name] = req
		}
	}
	return kept
}

func runTofu(tofu string, stdout, stderr io.Writer, args ...string) error {
	cmd := exec.Command(tofu, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = append(os.Environ(), "TF_IN_AUTOMATION=1")
	return cmd.Run()
}

func hasPlatformArg(args []string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "-platform=") || strings.HasPrefix(a, "--platform=") {
			return true
		}
	}
	return false
}

// rootRules is every tf_library and tf_deploy in the main repository (their
// underlying rules are `_tf_library` and `tf_deploy_rule`).
const rootRules = `kind("^(_tf_library|tf_deploy_rule) rule$", //...)`

// configQuery lists the configuration files of those rules, including those
// reached through filegroups and library deps, in any repository.
const configQuery = `filter("\.tf(\.json)?$", kind("source file", deps(` + rootRules + `)))`

// moduleQuery lists the libraries with a module_source among their deps:
// those are the modules, tf_modules' generated ones included, whose files
// may come from another repository.
const moduleQuery = `attr("module_source", ".", kind("^_tf_library rule$", deps(` + rootRules + `)))`

// queryPartialExitCode is Bazel's exit code for a --keep_going command that
// produced results despite errors.
const queryPartialExitCode = 3

// queryConfigFiles asks Bazel for the configuration files of every tf_library
// and tf_deploy in the main repository, and of the modules they depend on,
// grouped by directory: workspace-relative ("" for the workspace root) in the
// main repository, `@<repo>//<dir>` in others. The paths are absolute.
//
// A module from another repository, such as one tf_modules fetched, holds
// files its library doesn't serve, like examples requiring providers of
// their own. Only the directories tfconfig.ModuleDirs gives for it count.
//
// The query runs with --keep_going because `deps()` also follows each
// rule's `providers` into @tf_providers, which can't load before they are
// locked: before the first pin, `@tf_providers//<ns>/<name>` has no package,
// and a lock entry that can't be used makes its provider's repo fail when
// fetched. Neither has .tf files to find, and pin is how both are fixed.
func queryConfigFiles(bazel, workspace string, log io.Writer) (map[string][]string, error) {
	targets, err := query(bazel, workspace, configQuery+" + "+moduleQuery, log)
	if err != nil {
		return nil, err
	}
	var files, modules []target
	for _, t := range targets {
		if t.Kind == "source file" {
			files = append(files, t)
		} else {
			modules = append(modules, t)
		}
	}

	served := map[string]bool{}
	for _, m := range modules {
		if m.Repo == "" {
			continue
		}
		pkgDir := filepath.Dir(m.Path)
		root := pkgDir
		if m.Package != "" {
			root = strings.TrimSuffix(pkgDir, string(filepath.Separator)+filepath.FromSlash(m.Package))
		}
		dirs, err := tfconfig.ModuleDirs(root, m.Package)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", m.Label, err)
		}
		for _, d := range dirs {
			served[filepath.Join(root, filepath.FromSlash(d))] = true
		}
	}

	byDir := map[string][]string{}
	for _, f := range files {
		dir := path.Dir(path.Join(f.Package, f.Name))
		if dir == "." {
			dir = ""
		}
		if f.Repo != "" {
			if !served[filepath.Dir(f.Path)] {
				continue
			}
			dir = "@" + f.Repo + "//" + dir
		}
		byDir[dir] = append(byDir[dir], f.Path)
	}
	return byDir, nil
}

// target is one line of `bazel query --output=location`: a target's label,
// split up, its kind, and the absolute path of the file it is or is declared
// in.
type target struct {
	Kind, Label, Repo, Package, Name, Path string
}

// query runs a `bazel query --output=location`, accepting partial results.
func query(bazel, workspace, expr string, log io.Writer) ([]target, error) {
	cmd := exec.Command(bazel, "query", "--keep_going", "--noimplicit_deps", "--notool_deps", "--output=location", expr)
	cmd.Dir = workspace
	cmd.Stderr = log
	out, err := cmd.Output()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == queryPartialExitCode {
		fmt.Fprintln(log, "rules_tofu: bazel query couldn't load everything (above); locking against the configuration it could. "+
			"Providers that aren't locked yet are expected among the errors.")
	} else if err != nil {
		return nil, fmt.Errorf("bazel query: %w", err)
	}
	return parseLocations(string(out)), nil
}

// locationLine is `<path>:<line>:<column>: <kind> <label>`.
var locationLine = regexp.MustCompile(`^(.+):\d+:\d+: (.+) (\S+)$`)

// parseLocations reads `bazel query --output=location` output. `Repo` is ""
// for the main repository, else the repository's name as Bazel prints it.
func parseLocations(out string) []target {
	var targets []target
	for _, line := range strings.Split(out, "\n") {
		m := locationLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		label := m[3]
		repo, rest, ok := strings.Cut(strings.TrimLeft(label, "@"), "//")
		if !ok {
			continue
		}
		pkg, name, ok := strings.Cut(rest, ":")
		if !ok {
			continue
		}
		targets = append(targets, target{Kind: m[2], Label: label, Repo: repo, Package: pkg, Name: name, Path: m[1]})
	}
	return targets
}

var (
	terraformSchema = &hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: "terraform"}}}
	requiredSchema  = &hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: "required_providers"}}}
)

// parseFiles returns the required_providers entries declared in the given
// files, which share a directory, keyed by local name.
func parseFiles(files []string) (map[string]requirement, error) {
	parser := hclparse.NewParser()
	reqs := map[string]requirement{}
	where := map[string]string{}
	sort.Strings(files)
	for _, path := range files {
		src, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var file *hcl.File
		var diags hcl.Diagnostics
		if strings.HasSuffix(path, ".json") {
			file, diags = parser.ParseJSON(src, path)
		} else {
			file, diags = parser.ParseHCL(src, path)
		}
		if diags.HasErrors() {
			return nil, diags
		}
		content, _, diags := file.Body.PartialContent(terraformSchema)
		if diags.HasErrors() {
			return nil, diags
		}
		for _, tf := range content.Blocks {
			inner, _, diags := tf.Body.PartialContent(requiredSchema)
			if diags.HasErrors() {
				return nil, diags
			}
			for _, block := range inner.Blocks {
				attrs, diags := block.Body.JustAttributes()
				if diags.HasErrors() {
					return nil, diags
				}
				for name, attr := range attrs {
					req, err := parseRequirement(name, attr.Expr)
					if err != nil {
						return nil, fmt.Errorf("%s: %w", attr.Range, err)
					}
					if prior, ok := where[name]; ok {
						return nil, fmt.Errorf("%s: provider %q is already required in %s", attr.Range, name, prior)
					}
					where[name] = attr.Range.String()
					reqs[name] = req
				}
			}
		}
	}
	return reqs, nil
}

// parseRequirement reads one entry: `{ source = ..., version = ... }`, or the
// legacy bare version string (with the source implied, as tofu does).
// Other keys, like configuration_aliases, are references that can't be
// evaluated here and don't affect locking, so they're skipped.
func parseRequirement(name string, expr hcl.Expression) (requirement, error) {
	req := requirement{Source: "hashicorp/" + name}
	pairs, diags := hcl.ExprMap(expr)
	if diags.HasErrors() {
		version, ok := literalString(expr)
		if !ok {
			return req, fmt.Errorf("required_providers entry %q is neither an object nor a version string", name)
		}
		req.Version = version
		return req, nil
	}
	for _, pair := range pairs {
		key, ok := literalString(pair.Key)
		if !ok || (key != "source" && key != "version") {
			continue
		}
		value, ok := literalString(pair.Value)
		if !ok {
			return req, fmt.Errorf("`%s` of required_providers entry %q must be a literal string", key, name)
		}
		if key == "source" {
			req.Source = value
		} else {
			req.Version = value
		}
	}
	return req, nil
}

// literalString evaluates expr with no variables in scope, as a string.
func literalString(expr hcl.Expression) (string, bool) {
	v, diags := expr.Value(nil)
	if diags.HasErrors() || v.IsNull() || !v.IsKnown() || v.Type().FriendlyName() != "string" {
		return "", false
	}
	return v.AsString(), true
}

// moduleNames gives each child module a unique, valid module name derived
// from its directory, e.g. `examples/lock/greeting` -> `examples_lock_greeting`.
func moduleNames(children []module) []string {
	names := make([]string, len(children))
	used := map[string]bool{}
	for i, m := range children {
		var b strings.Builder
		for _, r := range m.Dir {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
				b.WriteRune(r)
			} else {
				b.WriteRune('_')
			}
		}
		base := b.String()
		if base == "" || !(base[0] >= 'a' && base[0] <= 'z' || base[0] >= 'A' && base[0] <= 'Z') {
			base = "m_" + base
		}
		name := base
		for n := 2; used[name]; n++ {
			name = fmt.Sprintf("%s_%d", base, n)
		}
		used[name] = true
		names[i] = name
	}
	return names
}

// writeScratch writes the configuration that `providers lock` runs against:
// each directory's requirements in a child module of its own, so tofu
// combines them exactly as it would for nested modules.
func writeScratch(scratch string, children []module) error {
	rootDoc := map[string]any{}
	calls := map[string]any{}
	for i, name := range moduleNames(children) {
		dir := filepath.Join(scratch, "modules", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		doc := map[string]any{"terraform": map[string]any{"required_providers": children[i].Reqs}}
		if err := writeJSON(filepath.Join(dir, "main.tf.json"), doc); err != nil {
			return err
		}
		calls[name] = map[string]string{"source": "./modules/" + name}
	}
	if len(calls) > 0 {
		rootDoc["module"] = calls
	}
	return writeJSON(filepath.Join(scratch, "main.tf.json"), rootDoc)
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// constraintsSummary lists every directory's constraint, by provider, so a
// conflict tofu reports without naming its sources can be traced.
func constraintsSummary(modules []module) string {
	type entry struct{ dir, version string }
	bySource := map[string][]entry{}
	add := func(m module) {
		dir := m.Dir
		if dir == "" {
			dir = "(workspace root)"
		}
		for _, req := range m.Reqs {
			source := strings.ToLower(req.Source)
			version := req.Version
			if version == "" {
				version = "(any)"
			}
			bySource[source] = append(bySource[source], entry{dir, version})
		}
	}
	for _, m := range modules {
		add(m)
	}
	sources := make([]string, 0, len(bySource))
	for s := range bySource {
		sources = append(sources, s)
	}
	sort.Strings(sources)

	var b strings.Builder
	b.WriteString("rules_tofu: version constraints in the workspace, by provider:\n")
	for _, s := range sources {
		fmt.Fprintf(&b, "  %s\n", s)
		width := 0
		for _, e := range bySource[s] {
			width = max(width, len(e.dir))
		}
		for _, e := range bySource[s] {
			fmt.Fprintf(&b, "    %-*s  %s\n", width, e.dir, e.version)
		}
	}
	return b.String()
}
