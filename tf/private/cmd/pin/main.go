// Command pin is `bazel run @tf_providers//:pin`: it rewrites the workspace's
// .terraform.lock.hcl (tf_providers.from_lock) with `tofu providers lock`.
//
// The lock serves every tf_library and tf_deploy in the workspace, so it is
// locked against all of their version constraints, not just those beside it:
//
//   - The workspace is walked for directories holding .tf/.tf.json files,
//     skipping hidden directories, `.bazelignore`d paths and nested Bazel
//     workspaces (their own MODULE.bazel, REPO.bazel or WORKSPACE).
//   - Each directory's `terraform { required_providers { ... } }` entries are
//     extracted, and nothing else, so resources, variables and module calls
//     that only make sense in place never need to load.
//   - Providers the root module pins with tf_providers.provider tags
//     (--exclude-source) are left out: the lock mustn't pin them too.
//   - A scratch configuration is generated: the lock's directory is the root
//     module, every other directory a child module. A versions.tf beside the
//     lock thereby pins the versions; without one, tofu picks the newest
//     version that every directory accepts.
//   - `tofu get` + `tofu providers lock` run there, from scratch: `providers
//     lock` keeps an existing lock's versions and has no -upgrade. The new
//     lock is copied back over the old one.
//
// When the constraints can't all be met, tofu's error doesn't say where they
// came from, so pin lists them by provider and directory.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
)

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

var (
	tofu      = flag.String("tofu", "", "path to the tofu binary")
	lockDir   = flag.String("lock-dir", "", "workspace-relative directory of the .terraform.lock.hcl")
	platforms stringList
	excluded  stringList
)

func init() {
	flag.Var(&platforms, "platform", "`<os>_<arch>` to lock for unless -platform= arguments follow (repeatable)")
	flag.Var(&excluded, "exclude-source", "`<namespace>/<name>` pinned by a tag instead, left out of the lock (repeatable)")
}

func main() {
	flag.Parse()
	workspace := os.Getenv("BUILD_WORKSPACE_DIRECTORY")
	if workspace == "" {
		fmt.Fprintln(os.Stderr, "rules_tofu: pin must be invoked via `bazel run @tf_providers//:pin`")
		os.Exit(1)
	}
	if err := run(workspace, *lockDir, *tofu, platforms, excluded, flag.Args(), os.Stderr); err != nil {
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

// module is the requirements found in one workspace directory.
type module struct {
	Dir  string // workspace-relative, "" for the workspace root
	Reqs map[string]requirement
}

func run(workspace, lockDir, tofu string, defaultPlatforms, excludedSources, extraArgs []string, log io.Writer) error {
	dirs, err := discover(workspace)
	if err != nil {
		return err
	}
	lockDir = filepath.ToSlash(filepath.Clean(lockDir))
	if lockDir == "." {
		lockDir = ""
	}

	root := module{Dir: lockDir, Reqs: map[string]requirement{}}
	var children []module
	for _, dir := range dirs {
		reqs, err := parseDir(filepath.Join(workspace, dir))
		if err != nil {
			return err
		}
		reqs = withoutSources(reqs, excludedSources)
		if dir == lockDir {
			root.Reqs = reqs
		} else if len(reqs) > 0 {
			children = append(children, module{Dir: dir, Reqs: reqs})
		}
	}
	if len(root.Reqs) == 0 && len(children) == 0 {
		return fmt.Errorf("no required_providers found under %s to lock", workspace)
	}

	scratch, err := os.MkdirTemp("", "rules_tofu-pin-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	if err := writeScratch(scratch, root, children); err != nil {
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
		fmt.Fprint(log, constraintsSummary(root, children))
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
	n := len(children)
	if len(root.Reqs) > 0 {
		n++
	}
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

// isConfigFile reports whether name is a file tofu reads as configuration.
func isConfigFile(name string) bool {
	return strings.HasSuffix(name, ".tf") || strings.HasSuffix(name, ".tf.json")
}

// workspaceMarkers make a directory the root of another Bazel workspace,
// whose .tf files belong to it, not to this one.
var workspaceMarkers = []string{"MODULE.bazel", "REPO.bazel", "WORKSPACE", "WORKSPACE.bazel"}

// discover returns the workspace-relative directories ("" for the root) that
// hold configuration files, sorted.
func discover(workspace string) ([]string, error) {
	ignored, err := readBazelignore(workspace)
	if err != nil {
		return nil, err
	}
	found := map[string]bool{}
	err = filepath.WalkDir(workspace, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(workspace, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == "." {
				return nil
			}
			// WalkDir doesn't follow symlinks, so bazel-* convenience links are
			// never entered; hidden directories (.git, .terraform) are skipped.
			if strings.HasPrefix(d.Name(), ".") || ignored[rel] {
				return filepath.SkipDir
			}
			for _, marker := range workspaceMarkers {
				if _, err := os.Stat(filepath.Join(path, marker)); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if d.Type().IsRegular() && isConfigFile(d.Name()) {
			dir := filepath.ToSlash(filepath.Dir(rel))
			if dir == "." {
				dir = ""
			}
			found[dir] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	dirs := make([]string, 0, len(found))
	for dir := range found {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs, nil
}

// readBazelignore returns the workspace-relative paths listed in .bazelignore.
func readBazelignore(workspace string) (map[string]bool, error) {
	ignored := map[string]bool{}
	f, err := os.Open(filepath.Join(workspace, ".bazelignore"))
	if errors.Is(err, os.ErrNotExist) {
		return ignored, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		ignored[strings.Trim(filepath.ToSlash(filepath.Clean(line)), "/")] = true
	}
	return ignored, scanner.Err()
}

var (
	terraformSchema = &hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: "terraform"}}}
	requiredSchema  = &hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: "required_providers"}}}
)

// parseDir returns the required_providers entries declared in dir's
// configuration files, keyed by local name.
func parseDir(dir string) (map[string]requirement, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	parser := hclparse.NewParser()
	reqs := map[string]requirement{}
	where := map[string]string{}
	for _, e := range entries {
		if !e.Type().IsRegular() || !isConfigFile(e.Name()) {
			continue
		}
		path := filepath.Join(dir, e.Name())
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
// root's requirements in the root module, each child's in a module of its
// own, so tofu combines them exactly as it would for nested modules.
func writeScratch(scratch string, root module, children []module) error {
	rootDoc := map[string]any{}
	if len(root.Reqs) > 0 {
		rootDoc["terraform"] = map[string]any{"required_providers": root.Reqs}
	}
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
func constraintsSummary(root module, children []module) string {
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
	add(root)
	for _, m := range children {
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
