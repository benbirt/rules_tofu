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
//   - --modules=<path>: a JSON object {modules, packages}. `modules` lists
//     the tf_library deps that set `module_source`, each {label, source,
//     version, dir, direct}, where `dir` is the library's work-tree-relative
//     directory; `packages` is the work-tree-relative directory of every
//     tf_library package in deps, transitively.
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
//
// A direct dep counts as used only when the root's own configuration calls
// it: the root module, or a local module in the root's own package. A
// directory belongs to the package nearest above it, as in Bazel, so a local
// call into another library's package leaves the root's configuration; calls
// from there are that library's business and use none of the root's direct
// deps. A local call into a package uses the deps with module_source there,
// and the deps in the root's own package are used, since their files are part
// of the root module.
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

	"github.com/benbirt/rules_tofu/tf/private/cmd/internal/tfconfig"
	"github.com/hashicorp/hcl/v2"
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
	var listing struct {
		Modules  []dep    `json:"modules"`
		Packages []string `json:"packages"`
	}
	if err := json.Unmarshal(data, &listing); err != nil {
		return fmt.Errorf("parse %s: %w", *modules, err)
	}
	manifest, err := build(*workTree, *root, *label, listing.Modules, listing.Packages)
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

// The deepest module tree accepted, so a module that calls itself fails
// rather than looping.
const maxDepth = 64

func build(workTree, root, label string, deps []dep, packages []string) (*Manifest, error) {
	bySource := map[string]*dep{}
	var all []*dep
	for i := range deps {
		d := &deps[i]
		key := tfconfig.NormalizeSource(d.Source)
		if other, ok := bySource[key]; ok && other.Label != d.Label {
			return nil, fmt.Errorf("%s: %s and %s both have module_source %q; a source can be provided by one tf_library only",
				label, other.Label, d.Label, d.Source)
		}
		bySource[key] = d
		all = append(all, d)
	}

	used := map[string]bool{}
	for _, d := range all {
		if d.Dir == root {
			used[d.Label] = true
		}
	}
	manifest := &Manifest{Modules: []Record{{Key: "", Source: "", Dir: "."}}}
	// own is whether dir is the root's own configuration (see the package doc).
	var walk func(key, dir string, own bool, depth int) error
	walk = func(key, dir string, own bool, depth int) error {
		if depth > maxDepth {
			return fmt.Errorf("%s: module %q is nested more than %d deep; does a module call itself?", label, key, maxDepth)
		}
		calls, err := tfconfig.ReadCalls(filepath.Join(workTree, filepath.FromSlash(dir)))
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
			childOwn := false
			if tfconfig.IsLocal(c.Source) {
				childDir = path.Join(dir, c.Source)
				if err := checkLocal(workTree, dir, childDir); err != nil {
					return fmt.Errorf("%s: %s: source %q %w", label, where, c.Source, err)
				}
				if pkg := packageOf(packages, root, childDir); pkg == root {
					childOwn = own
				} else if own {
					for _, d := range all {
						if d.Dir == pkg {
							used[d.Label] = true
						}
					}
				}
			} else {
				d, subdir := lookup(all, c.Source)
				if d == nil {
					return fmt.Errorf("%s: %s: no dep provides source %q. %s", label, where, c.Source, missingHint(c.Source))
				}
				if own {
					used[d.Label] = true
				}
				childDir = path.Join(d.Dir, subdir)
				if subdir != "" && !isDir(workTree, childDir) {
					return fmt.Errorf("%s: %s: %s (module_source %q) has no directory %q", label, where, d.Label, d.Source, subdir)
				}
				if tfconfig.IsRegistry(c.Source) {
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
			if err := walk(childKey, childDir, childOwn, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk("", root, true, 0); err != nil {
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

// packageOf is the package dir belongs to: the nearest of packages, or
// root, at or above it, as Bazel gives a directory to the nearest package
// above it.
func packageOf(packages []string, root, dir string) string {
	best, found := "", false
	for _, p := range append([]string{root}, packages...) {
		if within(dir, p) && (!found || len(p) > len(best)) {
			best, found = p, true
		}
	}
	return best
}

// within is whether dir is ancestor or inside it, "" being the work tree
// root, which contains everything.
func within(dir, ancestor string) bool {
	return ancestor == "" || dir == ancestor || strings.HasPrefix(dir, ancestor+"/")
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

// lookup finds the dep providing source (see tfconfig.Match) and the
// subdirectory of the dep's directory that the call uses. When several deps
// match, the one with the longest module_source wins, so a submodule's own
// library takes precedence over the module containing it.
func lookup(deps []*dep, source string) (*dep, string) {
	var best *dep
	var bestSubdir string
	for _, d := range deps {
		subdir, ok := tfconfig.Match(d.Source, source)
		if ok && (best == nil || len(d.Source) > len(best.Source)) {
			best, bestSubdir = d, subdir
		}
	}
	return best, bestSubdir
}

func missingHint(source string) string {
	base, _ := tfconfig.SplitSubdir(source)
	if s, err := tfconfig.ParseSource(source); err == nil && s.Kind == tfconfig.KindRegistry {
		label := strings.ToLower(s.Namespace + "/" + s.Name + "/" + s.System)
		if s.Host != tfconfig.DefaultRegistryHost {
			label = s.Host + "/" + label
		}
		return fmt.Sprintf("Declare it in MODULE.bazel with tf_modules.module(source = %q, version = ...) and add "+
			"\"@tf_modules//%s\" to deps, or add a tf_library with module_source = %q.", base, label, base)
	}
	return fmt.Sprintf("Declare it in MODULE.bazel with tf_modules.module(name = ..., source = %q, ...) and add "+
		"\"@tf_modules//<name>\" to deps, or add a tf_library with module_source = %q.", source, source)
}

func displayRange(workTree string, r hcl.Range) string {
	name := r.Filename
	if rel, err := filepath.Rel(workTree, name); err == nil {
		name = filepath.ToSlash(rel)
	}
	return fmt.Sprintf("%s:%d", name, r.Start.Line)
}
