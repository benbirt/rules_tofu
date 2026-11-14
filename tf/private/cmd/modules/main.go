// Command modules writes the .terraform/modules/modules.json manifest for a
// materialised work tree, the record `tofu init` keeps of installed module
// calls. With the manifest in place, `tofu init -get=false` fetches and
// installs nothing and still checks each call's source and version against it.
//
// It runs as a Bazel build action for tf_library, tf_deploy and tf_test.
//
// Flags:
//   - --work-tree=<path>: work tree root, required.
//   - --root=<dir>: root module directory relative to --work-tree, or "" for
//     the workspace root package.
//   - --modules=<path>: required JSON file with fields modules and packages.
//     modules lists tf_library deps that set module_source, each with label,
//     source, version, work-tree-relative dir and direct. When the library
//     being built sets module_source, it appears too with direct false so its
//     files can call its own address with //<subdir>. packages lists the
//     work-tree-relative directory of every transitive tf_library package in
//     deps.
//   - --provider=<address>: canonical <host>/<namespace>/<name> address of a
//     provider in the rule's plugin tree, repeatable.
//   - --test-dir=<dir>: for tf_test, the test directory relative to --root, or
//     "." for --root itself. Each `run` block's module in test files in --root
//     and in --test-dir is recorded under tfconfig.TestRunKey and walked like
//     a call from the root module.
//   - --label=<label>: target label for diagnostics, required.
//   - --out=<path>: manifest path to write, required.
//
// Starting at --root, modules walks every `module` block and any test `run`
// modules and writes {"Modules":[...]} sorted by Key to --out. Local calls are
// recorded too, since `tofu init -get=false` installs none.
//
// It rejects the build when:
//   - a local source starting with ./, ../, .\ or ..\ leaves the work tree,
//     crosses into or out of external/<repo> or names a directory absent from
//     the work tree;
//   - a non-local source matches no dep (see tfconfig.BestMatch), matches a
//     registry dep that omits module_version or lands in a directory with no
//     configuration files;
//   - two deps claim the same normalised module_source;
//   - module calls form a cycle;
//   - a direct dep is used by no call;
//   - a walked module's required_providers names a provider that no
//     --provider supplies, since `tofu init -plugin-dir` would fail to find
//     it.
//
// Missing-provider errors name the @tf_providers//<namespace>/<name> label to
// add to the rule's providers. Built-in providers under terraform.io/builtin/
// and opentofu.org/builtin/ need no plugin and are skipped. A tf_library
// carries the providers listed on it and its deps, checked from its own root
// module; for a fetched module, modscan populates that list from its served
// directories. Neither reads //<subdir> paths outside modules/ that only
// callers reach (issue #76), so callers must list those providers themselves.
//
// A direct dep counts as used only when the root's own configuration calls it:
// the root module, or a local module in the root's own package. Deps in the
// root's own package are always used, since their files are part of the root
// module. Each directory belongs to the deepest entry of packages or --root at
// or above it, matching Bazel's package boundaries. A local call into another
// library's package marks any module_source deps in that package used and
// leaves the root's configuration, since calls from there belong to that
// library's own deps. A module_source call that lands in the root's own
// package stays in the root's configuration like a local call.
//
// Unrecognised or malformed flags exit 2. Missing required flags, unreadable
// or invalid JSON or HCL, I/O errors or any failed check print
// rules_tofu: <error> to stderr and exit 1.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/benbirt/rules_tofu/tf/private/cmd/internal/flags"
	"github.com/benbirt/rules_tofu/tf/private/cmd/internal/tfconfig"
	"github.com/hashicorp/hcl/v2"
	regaddr "github.com/opentofu/registry-address/v2"
)

var (
	workTree = flag.String("work-tree", "", "path to the work tree root")
	root     = flag.String("root", "", "work-tree-relative directory of the root module")
	modules  = flag.String("modules", "", "JSON file listing the deps that set module_source")
	label    = flag.String("label", "", "the target being built, for error messages")
	out      = flag.String("out", "", "path of the modules.json to write")
	provided = flags.StringList("provider", "canonical address of a provider in the plugin tree (repeatable)")
	testDir  = flag.String("test-dir", "", "directory of the rule's test files, relative to the root module (\".\" for its own directory); when set, the module each run block of the test files there and in the root module's directory loads is recorded too")
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
	manifest, err := buildManifest(*workTree, *root, *label, listing.Modules, listing.Packages, *provided, *testDir)
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
// names from the root, or "" for the root itself; Dir is slash-separated and
// relative to the root module.
type Record struct {
	Key     string `json:"Key"`
	Source  string `json:"Source"`
	Version string `json:"Version,omitempty"`
	Dir     string `json:"Dir"`
}

// requirement is a provider a module requires that no --provider supplies.
type requirement struct {
	// module names the module: moduleName(key), then for a module_source
	// call the dep's label and the `//<subdir>` used, in parentheses.
	module   string
	provider string
}

// build is buildManifest for a rule with no test files.
func build(workTree, root, label string, deps []dep, packages []string, provided []string) (*Manifest, error) {
	return buildManifest(workTree, root, label, deps, packages, provided, "")
}

// buildManifest constructs the manifest for the root module at root in
// workTree. testDir is "" for a rule with no test files, or the test
// directory relative to root, with "." meaning root itself. When testDir is
// non-empty, each `run` block's module in test files in root and in testDir
// is recorded as `tofu init` records it and walked like a call from root.
func buildManifest(workTree, root, label string, deps []dep, packages []string, provided []string, testDir string) (*Manifest, error) {
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
	supplied := map[string]bool{}
	for _, p := range provided {
		supplied[p] = true
	}
	var missing []requirement
	// reported keeps a module that two calls reach from being reported twice.
	reported := map[string]bool{}
	// require records the providers that dir's configuration requires and no
	// --provider supplies; module names the module (see requirement).
	require := func(dir, module string) error {
		providers, err := tfconfig.RequiredProviders(filepath.Join(workTree, filepath.FromSlash(dir)))
		if err != nil {
			return err
		}
		for _, p := range providers {
			if supplied[p] || isBuiltin(p) || reported[dir+" "+p] {
				continue
			}
			reported[dir+" "+p] = true
			missing = append(missing, requirement{module: module, provider: p})
		}
		return nil
	}
	// active is the directory of each module on the path from the root to the
	// one being read, by its key. A call back into one of them is a cycle,
	// reported at once: tofu rejects it too, and short of one a module tree
	// is finite, so no depth limit is needed.
	active := map[string]string{}
	// own is whether dir is the root's own configuration (see the package doc);
	// module names the module at dir, for a missing provider.
	var walk func(key, dir, module string, own bool) error
	// visit records the call c, made from dir under the key childKey, and
	// walks into the module it loads. where names the call in errors.
	visit := func(c tfconfig.Call, childKey, where, dir string, own bool) error {
		record := Record{Key: childKey, Source: c.Source}
		var childDir string
		childOwn := false
		childModule := moduleName(childKey)
		if tfconfig.IsLocal(c.Source) {
			childDir = tfconfig.ResolveLocal(dir, c.Source)
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
			// A library's call to its own module_source with `//<subdir>`
			// stays in its package, as a local path there would: what that
			// module calls is still the root's own business.
			if packageOf(packages, root, childDir) == root {
				childOwn = own
			}
			if subdir == "" {
				childModule += fmt.Sprintf(" (%s)", d.Label)
			} else {
				// The call's own `//<subdir>`, as written, not the part
				// below the dep's module_source.
				_, called := tfconfig.SplitSubdir(c.Source)
				childModule += fmt.Sprintf(" (%s, //%s)", d.Label, called)
			}
			if err := checkProvided(workTree, childDir, subdir); err != nil {
				return fmt.Errorf("%s: %s: %s (module_source %q) %w", label, where, d.Label, d.Source, err)
			}
			if tfconfig.IsRegistry(c.Source) {
				if d.Version == "" {
					return fmt.Errorf("%s: %s: %s provides the registry module %q, so it must set module_version", label, where, d.Label, d.Source)
				}
				record.Version = d.Version
			}
		}
		if above, ok := active[childDir]; ok {
			return fmt.Errorf("%s: %s: source %q is the directory of %s, an ancestor of this call, so the module calls form a cycle",
				label, where, c.Source, moduleName(above))
		}
		rel, err := filepath.Rel(filepath.FromSlash(rootDir(root)), filepath.FromSlash(rootDir(childDir)))
		if err != nil {
			return err
		}
		record.Dir = filepath.ToSlash(rel)
		manifest.Modules = append(manifest.Modules, record)
		return walk(childKey, childDir, childModule, childOwn)
	}
	walk = func(key, dir, module string, own bool) error {
		active[dir] = key
		defer delete(active, dir)
		if err := require(dir, module); err != nil {
			return err
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
			if err := visit(c, childKey, where, dir, own); err != nil {
				return err
			}
		}
		return nil
	}
	// A root module with no file at or below its directory has no directory
	// in the work tree, and one with no files at all has no work tree. Treat
	// it as an empty module, as when the directory exists (issue #119); any
	// other stat error is still reported.
	switch _, err := os.Stat(filepath.Join(workTree, filepath.FromSlash(root))); {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := walk("", root, moduleName(""), true); err != nil {
			return nil, err
		}
	}
	if testDir != "" {
		runs, err := testRuns(workTree, root, testDir)
		if err != nil {
			return nil, err
		}
		// A run's module is loaded from the root module's directory, as tofu
		// resolves its source, and is the root's own call: a dep it uses is
		// used, and a module it loads that calls back into the root is a cycle.
		active[root] = ""
		for _, r := range runs {
			where := fmt.Sprintf("run %q (%s)", r.Name, displayRange(workTree, r.Range))
			if err := visit(r.Call, r.key, where, root, true); err != nil {
				return nil, err
			}
		}
		delete(active, root)
	}
	if len(missing) > 0 {
		return nil, missingProviders(label, missing)
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

// testRun is the module a test file's `run` block loads, under the key tofu
// records it by (see tfconfig.TestRunKey).
type testRun struct {
	key string
	tfconfig.Call
}

// testRuns reads the test files `tofu test` reads for the root module: those
// in its directory and, unless testDir is ".", those in testDir below it.
func testRuns(workTree, root, testDir string) ([]testRun, error) {
	rootAbs := filepath.Join(workTree, filepath.FromSlash(root))
	dirs := []string{rootAbs}
	if testDir != "." {
		dirs = append(dirs, filepath.Join(rootAbs, filepath.FromSlash(testDir)))
	}
	var runs []testRun
	for _, dir := range dirs {
		files, err := tfconfig.TestFiles(dir)
		if err != nil {
			return nil, err
		}
		calls, err := tfconfig.ReadTestCalls(files)
		if err != nil {
			return nil, err
		}
		for _, c := range calls {
			rel, err := filepath.Rel(rootAbs, c.Range.Filename)
			if err != nil {
				return nil, err
			}
			runs = append(runs, testRun{key: tfconfig.TestRunKey(rel, c.Name), Call: c})
		}
	}
	return runs, nil
}

// missingProviders reports the providers that modules require and the plugin
// tree lacks, naming the `@tf_providers//<namespace>/<name>` of each to add
// to the rule's providers. A provider from another registry has no label:
// rules_tofu vendors providers from registry.opentofu.org only, as the
// tf_providers extension says of its tags.
func missingProviders(label string, missing []requirement) error {
	var lines, labels, foreign []string
	seen := map[string]bool{}
	for _, m := range missing {
		lines = append(lines, fmt.Sprintf("    %s requires %s", m.module, displayProvider(m.provider)))
		if seen[m.provider] {
			continue
		}
		seen[m.provider] = true
		if l, ok := providerLabel(m.provider); ok {
			labels = append(labels, fmt.Sprintf("%q", l))
		} else {
			foreign = append(foreign, m.provider)
		}
	}
	var advice []string
	if len(labels) > 0 {
		advice = append(advice, fmt.Sprintf("Add %s to the providers of %s.", strings.Join(labels, ", "), label))
	}
	if len(foreign) > 0 {
		advice = append(advice, fmt.Sprintf("rules_tofu can't vendor %s: it only vendors providers from %s.",
			strings.Join(foreign, ", "), defaultProviderHost))
	}
	return fmt.Errorf("%s: nothing in its providers or deps supplies these providers, so `tofu init` would not find them:\n%s\n%s",
		label, strings.Join(lines, "\n"), strings.Join(advice, " "))
}

// defaultProviderHost is the registry rules_tofu vendors providers from,
// which a provider address may leave out.
var defaultProviderHost = regaddr.DefaultProviderRegistryHost.String()

// isBuiltin reports whether canonical provider address provider is built into
// tofu, so that it needs no plugin.
func isBuiltin(provider string) bool {
	p, err := regaddr.ParseProviderSource(provider)
	return err == nil && p.IsBuiltIn()
}

// displayProvider is a canonical provider address as users write it: without
// the default host.
func displayProvider(provider string) string {
	return strings.TrimPrefix(provider, defaultProviderHost+"/")
}

// providerLabel is the `@tf_providers//<namespace>/<name>` label of a
// provider the tf_providers extension can vendor: one from the default host.
func providerLabel(provider string) (string, bool) {
	rest, ok := strings.CutPrefix(provider, defaultProviderHost+"/")
	if !ok {
		return "", false
	}
	return "@tf_providers//" + rest, true
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

// rootDir maps the work tree root "" to ".", for filepath.Rel.
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

// moduleName names a module by its manifest key, for error messages.
func moduleName(key string) string {
	if key == "" {
		return "the root module"
	}
	return fmt.Sprintf("module %q", key)
}

// checkProvided checks dir, which is subdir within a dep's directory, or ""
// for the dep's own directory. It must hold configuration files, as tofu
// requires of a module installed from a remote source. A dep's data can bring
// directories without configuration along, since a glob(["**"]) includes
// docs, and a dep with no files in its own directory has no directory in the
// work tree at all.
func checkProvided(workTree, dir, subdir string) error {
	if tfconfig.HasConfig(filepath.Join(workTree, filepath.FromSlash(dir))) {
		return nil
	}
	if subdir == "" {
		return errors.New("has no .tf, .tf.json, .tofu or .tofu.json files")
	}
	if !isDir(workTree, dir) {
		return fmt.Errorf("has no directory %q", subdir)
	}
	return fmt.Errorf("has no .tf, .tf.json, .tofu or .tofu.json files in %q", subdir)
}

// checkLocal checks a local call's target: a directory of the work tree,
// which is to say of some tf_library in deps, and in the same repository as
// the caller. Files from other repositories are under `external/`, at a path
// that depends on Bazel's canonical repository names, so they are reached by
// module_source instead. The directory may hold no configuration: tofu loads
// a local module from an empty directory, unlike a remote one.
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
// main repository, the name under external/ for external repositories or "/"
// for external itself so ../external never matches the main repository.
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

// lookup finds the dep providing source and the subdirectory of the dep's
// directory that the call uses. When several deps match, the most specific
// wins (see tfconfig.BestMatch), so a submodule's own library takes
// precedence over the module containing it.
func lookup(deps []*dep, source string) (*dep, string) {
	sources := make([]string, len(deps))
	for i, d := range deps {
		sources[i] = d.Source
	}
	i, subdir := tfconfig.BestMatch(sources, source)
	if i < 0 {
		return nil, ""
	}
	return deps[i], subdir
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
