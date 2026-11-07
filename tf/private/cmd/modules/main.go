// Command modules writes the `.terraform/modules/modules.json` manifest for a
// materialized work tree: the record `tofu init` keeps of every module call it
// has installed. With the manifest in place, `tofu init -get=false` installs
// nothing and fetches nothing, and still checks each call's `source` and
// `version` against it.
//
// It is invoked from a Bazel build action by tf_library and tf_deploy.
// Inputs:
//   - --work-tree=<path>: the work tree root.
//   - --root=<dir>: the root module's work-tree-relative directory.
//   - --modules=<path>: a JSON list of the tf_library deps that set
//     `module_source`, each {label, source, version, dir, direct}, where `dir`
//     is the library's work-tree-relative directory.
//   - --label=<label>: the target being built, for error messages.
//   - --out=<path>: the manifest to write.
//
// Starting at the root module, it reads every `module` block. A local source
// (`./` or `../`) is a directory of the work tree. Any other source must be
// the `module_source` of one of the deps, or that plus `//<subdir>`. Every
// call is recorded, local ones included, since -get=false installs none, and
// the walk continues into the called module.
//
// It fails when a call's source is provided by no dep, when a direct dep is
// used by no call, and when two deps claim the same source.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
)

var (
	workTree = flag.String("work-tree", "", "path to the work tree root")
	root     = flag.String("root", "", "work-tree-relative directory of the root module")
	modules  = flag.String("modules", "", "JSON file listing the deps that set module_source")
	label    = flag.String("label", "", "the target being built, for error messages")
	out      = flag.String("out", "", "path of the modules.json to write")
)

func main() {
	flag.Parse()
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "rules_tofu: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	for name, value := range map[string]string{
		"--work-tree": *workTree,
		"--modules":   *modules,
		"--label":     *label,
		"--out":       *out,
	} {
		if value == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	data, err := os.ReadFile(*modules)
	if err != nil {
		return err
	}
	var deps []dep
	if err := json.Unmarshal(data, &deps); err != nil {
		return fmt.Errorf("parse %s: %w", *modules, err)
	}
	manifest, err := build(*workTree, *root, *label, deps)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	return os.WriteFile(*out, append(encoded, '\n'), 0o644)
}

// dep is a tf_library that sets `module_source`.
type dep struct {
	Label   string `json:"label"`
	Source  string `json:"source"`
	Version string `json:"version"`
	Dir     string `json:"dir"`
	// Direct is whether the root lists it in deps itself, rather than through
	// another library. A direct dep must be used.
	Direct bool `json:"direct"`
}

// Manifest is tofu's `.terraform/modules/modules.json`.
type Manifest struct {
	Modules []Record `json:"Modules"`
}

// Record is one installed module call. Key is the dot-separated path of call
// names from the root (which is ""), Dir is relative to the root module.
type Record struct {
	Key     string `json:"Key"`
	Source  string `json:"Source"`
	Version string `json:"Version,omitempty"`
	Dir     string `json:"Dir"`
}

// call is a `module` block.
type call struct {
	Name   string
	Source string
	Range  hcl.Range
}

// The deepest module tree accepted, so a module that calls itself fails
// rather than looping.
const maxDepth = 64

func build(workTree, root, label string, deps []dep) (*Manifest, error) {
	bySource := map[string]*dep{}
	for i := range deps {
		d := &deps[i]
		key := NormalizeSource(d.Source)
		if other, ok := bySource[key]; ok && other.Label != d.Label {
			return nil, fmt.Errorf("%s: %s and %s both have module_source %q; a source can be provided by one tf_library only",
				label, other.Label, d.Label, d.Source)
		}
		bySource[key] = d
	}

	used := map[string]bool{}
	manifest := &Manifest{Modules: []Record{{Key: "", Source: "", Dir: "."}}}
	var walk func(key, dir string, depth int) error
	walk = func(key, dir string, depth int) error {
		if depth > maxDepth {
			return fmt.Errorf("%s: module %q is nested more than %d deep; does a module call itself?", label, key, maxDepth)
		}
		calls, err := readCalls(filepath.Join(workTree, filepath.FromSlash(dir)))
		if err != nil {
			return err
		}
		for _, c := range calls {
			childKey := c.Name
			if key != "" {
				childKey = key + "." + c.Name
			}
			where := fmt.Sprintf("module %q (%s)", c.Name, displayRange(workTree, c.Range))
			record := Record{Key: childKey, Source: c.Source}
			var childDir string
			if isLocal(c.Source) {
				childDir = path.Join(dir, c.Source)
				if err := checkLocal(workTree, dir, childDir); err != nil {
					return fmt.Errorf("%s: %s: source %q %w", label, where, c.Source, err)
				}
			} else {
				d, subdir := lookup(bySource, c.Source)
				if d == nil {
					return fmt.Errorf("%s: %s: no dep provides source %q. %s", label, where, c.Source, missingHint(c.Source))
				}
				used[d.Label] = true
				childDir = path.Join(d.Dir, subdir)
				if subdir != "" && !isDir(workTree, childDir) {
					return fmt.Errorf("%s: %s: %s (module_source %q) has no directory %q", label, where, d.Label, d.Source, subdir)
				}
				if isRegistry(c.Source) {
					if d.Version == "" {
						return fmt.Errorf("%s: %s: %s provides the registry module %q, so it must set module_version", label, where, d.Label, d.Source)
					}
					record.Version = d.Version
				}
			}
			rel, err := filepath.Rel(filepath.FromSlash(rootDir(root)), filepath.FromSlash(rootDir(childDir)))
			if err != nil {
				return err
			}
			record.Dir = filepath.ToSlash(rel)
			manifest.Modules = append(manifest.Modules, record)
			if err := walk(childKey, childDir, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk("", root, 0); err != nil {
		return nil, err
	}

	var unused []string
	for _, d := range deps {
		if d.Direct && !used[d.Label] {
			unused = append(unused, fmt.Sprintf("%s (module_source %q)", d.Label, d.Source))
		}
	}
	if len(unused) > 0 {
		sort.Strings(unused)
		return nil, fmt.Errorf("%s: no module call uses %s; call it with that source or remove it from deps",
			label, strings.Join(unused, ", "))
	}

	sort.Slice(manifest.Modules, func(i, j int) bool { return manifest.Modules[i].Key < manifest.Modules[j].Key })
	return manifest, nil
}

// rootDir maps the work tree root ("") to ".", for filepath.Rel.
func rootDir(dir string) string {
	if dir == "" {
		return "."
	}
	return dir
}

func isDir(workTree, dir string) bool {
	info, err := os.Stat(filepath.Join(workTree, filepath.FromSlash(dir)))
	return err == nil && info.IsDir()
}

// checkLocal checks a local call's target: a directory of the work tree,
// which is to say of some tf_library in deps, and in the same repository as
// the caller. Files from other repositories are under `external/`, at a path
// that depends on Bazel's canonical repository names, so they are reached by
// module_source instead.
func checkLocal(workTree, parent, child string) error {
	if child == ".." || strings.HasPrefix(child, "../") {
		return errors.New("is outside the work tree")
	}
	if repoOf(parent) != repoOf(child) {
		return errors.New("is in another Bazel repository; refer to its tf_library by its module_source instead")
	}
	if !isDir(workTree, child) {
		return errors.New("is not in the work tree; add the tf_library in that directory to deps")
	}
	return nil
}

// repoOf is the Bazel repository a work-tree directory belongs to: "" for the
// main repository, else the name under `external/`.
func repoOf(dir string) string {
	rest, ok := strings.CutPrefix(dir, "external/")
	if !ok {
		if dir == "external" {
			return "/"
		}
		return ""
	}
	name, _, _ := strings.Cut(rest, "/")
	return name
}

// lookup finds the dep providing source: one whose module_source is source
// itself, or else source without its `//<subdir>`, which is then returned.
func lookup(bySource map[string]*dep, source string) (*dep, string) {
	if d, ok := bySource[NormalizeSource(source)]; ok {
		return d, ""
	}
	base, subdir := splitSubdir(source)
	if subdir == "" {
		return nil, ""
	}
	if d, ok := bySource[NormalizeSource(base)]; ok {
		return d, subdir
	}
	return nil, ""
}

func missingHint(source string) string {
	if isRegistry(source) {
		base, _ := splitSubdir(source)
		return fmt.Sprintf("Declare it with tf_modules.module(source = %q, version = ...) and add its tf_library to deps, "+
			"or add a tf_library with module_source = %q.", base, base)
	}
	return fmt.Sprintf("Add a tf_library with module_source = %q to deps.", source)
}

func displayRange(workTree string, r hcl.Range) string {
	name := r.Filename
	if rel, err := filepath.Rel(workTree, name); err == nil {
		name = filepath.ToSlash(rel)
	}
	return fmt.Sprintf("%s:%d", name, r.Start.Line)
}

// isLocal reports whether tofu reads source as a local path.
func isLocal(source string) bool {
	return strings.HasPrefix(source, "./") || strings.HasPrefix(source, "../")
}

// splitSubdir splits off a source's `//<subdir>`, the way tofu does: the
// first `//` that isn't part of a `scheme://`.
func splitSubdir(source string) (string, string) {
	rest := source
	offset := 0
	if i := strings.Index(rest, "://"); i >= 0 {
		offset = i + 3
		rest = rest[offset:]
	}
	i := strings.Index(rest, "//")
	if i < 0 {
		return source, ""
	}
	base := source[:offset+i]
	subdir := rest[i+2:]
	// A query string (`?ref=`) belongs to the base address, not the subdir.
	if q := strings.Index(subdir, "?"); q >= 0 {
		base += subdir[q:]
		subdir = subdir[:q]
	}
	return base, strings.Trim(subdir, "/")
}

// The registry's default host, which a registry address may leave out.
const defaultRegistryHost = "registry.opentofu.org"

// isRegistry reports whether tofu reads source as a module registry address:
// `[<host>/]<namespace>/<name>/<system>[//<subdir>]`, and nothing go-getter
// would claim first.
func isRegistry(source string) bool {
	_, ok := registryParts(source)
	return ok
}

func registryParts(source string) ([]string, bool) {
	if isLocal(source) || strings.Contains(source, "::") || strings.Contains(source, "://") {
		return nil, false
	}
	base, _ := splitSubdir(source)
	if strings.Contains(base, "?") {
		return nil, false
	}
	parts := strings.Split(base, "/")
	switch len(parts) {
	case 3:
		// A namespace has no dots; `github.com/<owner>/<repo>` is a shorthand.
		if strings.Contains(parts[0], ".") {
			return nil, false
		}
		parts = append([]string{defaultRegistryHost}, parts...)
	case 4:
		if !strings.Contains(parts[0], ".") {
			return nil, false
		}
		// go-getter's shorthands for these hosts win over the registry.
		switch strings.ToLower(parts[0]) {
		case "github.com", "bitbucket.org":
			return nil, false
		}
	default:
		return nil, false
	}
	for _, p := range parts {
		if p == "" {
			return nil, false
		}
	}
	return parts, true
}

// NormalizeSource is the form in which two sources are compared: registry
// addresses gain the default host and are lower-cased, as tofu treats them,
// with the subdir kept as written. Other sources are compared as written.
func NormalizeSource(source string) string {
	parts, ok := registryParts(source)
	if !ok {
		return source
	}
	normalized := strings.ToLower(strings.Join(parts, "/"))
	if _, subdir := splitSubdir(source); subdir != "" {
		normalized += "//" + subdir
	}
	return normalized
}

var moduleSchema = &hcl.BodySchema{
	Blocks: []hcl.BlockHeaderSchema{{Type: "module", LabelNames: []string{"name"}}},
}

var sourceSchema = &hcl.BodySchema{
	Attributes: []hcl.AttributeSchema{{Name: "source"}},
}

// readCalls parses the module blocks of the .tf and .tf.json files in dir. An
// `_override` file's `source` replaces the one in the primary file, as tofu
// merges it.
func readCalls(dir string) ([]call, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	parser := hclparse.NewParser()
	var primary, overrides []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !(strings.HasSuffix(name, ".tf") || strings.HasSuffix(name, ".tf.json")) {
			continue
		}
		stem := strings.TrimSuffix(strings.TrimSuffix(name, ".json"), ".tf")
		if stem == "override" || strings.HasSuffix(stem, "_override") {
			overrides = append(overrides, name)
		} else {
			primary = append(primary, name)
		}
	}

	var calls []call
	index := map[string]int{}
	for pass, group := range [][]string{primary, overrides} {
		isOverride := pass == 1
		for _, name := range group {
			found, err := parseCalls(parser, filepath.Join(dir, name))
			if err != nil {
				return nil, err
			}
			for _, c := range found {
				i, seen := index[c.Name]
				switch {
				case isOverride && seen:
					if c.Source != "" {
						calls[i].Source = c.Source
						calls[i].Range = c.Range
					}
				case isOverride:
					// An override of a call that doesn't exist; tofu reports it.
				case seen:
					return nil, fmt.Errorf("%s: duplicate module call %q", displayRange(dir, c.Range), c.Name)
				default:
					index[c.Name] = len(calls)
					calls = append(calls, c)
				}
			}
		}
	}
	for _, c := range calls {
		if c.Source == "" {
			return nil, fmt.Errorf("%s: module %q has no source", displayRange(dir, c.Range), c.Name)
		}
	}
	return calls, nil
}

func parseCalls(parser *hclparse.Parser, file string) ([]call, error) {
	src, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var f *hcl.File
	var diags hcl.Diagnostics
	if strings.HasSuffix(file, ".json") {
		f, diags = parser.ParseJSON(src, file)
	} else {
		f, diags = parser.ParseHCL(src, file)
	}
	if diags.HasErrors() {
		return nil, diags
	}
	content, _, diags := f.Body.PartialContent(moduleSchema)
	if diags.HasErrors() {
		return nil, diags
	}
	var calls []call
	for _, block := range content.Blocks {
		attrs, _, diags := block.Body.PartialContent(sourceSchema)
		if diags.HasErrors() {
			return nil, diags
		}
		c := call{Name: block.Labels[0], Range: block.DefRange}
		if attr, ok := attrs.Attributes["source"]; ok {
			source, ok := literalString(attr.Expr)
			if !ok {
				return nil, fmt.Errorf("%s: module %q: rules_tofu needs `source` to be a literal string",
					attr.Range, c.Name)
			}
			c.Source = source
			c.Range = attr.Range
		}
		calls = append(calls, c)
	}
	return calls, nil
}

// literalString evaluates expr with no variables in scope, as a string.
func literalString(expr hcl.Expression) (string, bool) {
	v, diags := expr.Value(nil)
	if diags.HasErrors() || v.IsNull() || !v.IsKnown() || v.Type().FriendlyName() != "string" {
		return "", false
	}
	return v.AsString(), true
}
