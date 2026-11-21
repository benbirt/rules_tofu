// Command modules writes the .terraform/modules/modules.json manifest for a
// materialised work tree, the record `tofu init` keeps of installed module
// calls. With the manifest in place, `tofu init -get=false` fetches and
// installs nothing and still checks each call's source and version against it.
//
// It runs as the TofuModules build action for tf_library, tf_deploy and
// tf_test.
//
// Flags:
//   - --tofu=<path>: path to the `tofu` binary, required.
//   - --work-tree=<path>: work tree root, required.
//   - --root=<dir>: root module directory relative to --work-tree, or "" for
//     the workspace root package.
//   - --modules=<path>: required JSON file with fields modules, own_srcs,
//     own_data and dep_dirs. modules lists tf_library deps that set
//     module_source, each with label, source, version, work-tree-relative
//     dir, dirs, direct and callable. When the library being built sets
//     module_source, it appears with direct false and callable true so its
//     files can call its address with //<subdir>. own_srcs and own_data are
//     work-tree paths of the target's srcs and data. dep_dirs lists the
//     work-tree directories of direct deps' srcs and data.
//   - --direct-provider=<address>: canonical <host>/<namespace>/<name> address
//     of a provider in the target's providers attribute, repeatable.
//   - --test-dir=<dir>: for tf_test, the test directory relative to --root, or
//     "." for --root itself. Each `run` block's module in test files in --root
//     and in --test-dir is recorded under tfconfig.TestRunKey and walked like
//     a call from the root module.
//   - --label=<label>: target label for diagnostics, required.
//   - --out=<path>: manifest path to write, required.
//
// Starting at --root, modules walks every `module` block and any test `run`
// modules and writes {"Modules":[...]} sorted by Key to --out. Local calls are
// recorded too, since `tofu init -get=false` installs none. Each walked
// directory is read once via tofushow.Reader.ReadFiles (`tofu show -json
// -module`, without `tofu init` or network access), passing every configuration
// file in that work-tree directory so it matches what `tofu init` loads.
// OpenTofu merges override files, lets .tofu and .tofu.json shadow .tf and
// .tf.json, and evaluates `module` `source` expressions using local values and
// variable defaults (vars and var_files are not passed). OpenTofu has no
// command that reports test `run` blocks, so test files are read with
// tfconfig.TestFiles and tfconfig.ReadTestCalls, where `module` `source` must
// be a literal string.
//
// It rejects the build when:
//   - OpenTofu cannot load a walked directory or the target's own files in a
//     directory;
//   - a local source starting with ./, ../, .\ or ..\ leaves the work tree,
//     crosses into or out of external/<repo> or names a directory absent from
//     the work tree;
//   - a non-local source matches no dep (see tfconfig.BestMatch), matches a
//     registry dep that omits module_version or lands in a directory with no
//     configuration files;
//   - two deps claim the same normalised module_source;
//   - module calls form a cycle;
//   - a direct module_source dep outside --root is not used by any call in the
//     target's checked files.
//
// Strict deps (issues #135 and #153) checks the target's own_srcs against its
// direct declarations. Configuration files in own_srcs are read directory by
// directory via tofushow.Reader.ReadFiles, each directory staged without other
// targets' files. Each target's files in a directory must stand alone: an
// override file (tfconfig.IsOverride) must be in the same target as the block
// it overrides, a `module` `source` referring to a local value or variable
// needs that declaration in the same target, a provider's required_providers
// entry must be in the same target as the files that use it (otherwise
// OpenTofu reads the local name as hashicorp/<name>), and a .tofu or
// .tofu.json file in another target does not shadow this target's .tf or
// .tf.json file during strict deps. On tf_test, `run` blocks in test files
// listed in own_srcs are checked as direct calls; test files from the module
// under test are not. In checked files, the build also fails on:
//   - a non-built-in provider reported by OpenTofu (in required_providers or
//     implied by a resource, data source or provider block) with no matching
//     --direct-provider;
//   - a non-local call resolving to a dep with callable false;
//   - a local call resolving to a directory outside own_srcs, own_data and
//     dep_dirs.
//
// Missing-provider errors name the @tf_providers//<namespace>/<name> label to
// add to providers, or report that a provider outside registry.opentofu.org
// cannot be vendored. Built-in providers under terraform.io/builtin/ and
// opentofu.org/builtin/ need no plugin and are skipped.
//
// Files from deps are not checked here, whether in --root or in modules
// reached by calls (issue #144). No rule takes configuration as data (issue
// #140), so every configuration file belongs to one target's srcs, and each
// dep is checked before dependants build (issue #143). Provider failures that
// depend on the assembled root module, such as a lock constraint mismatch or
// an empty-root tf_test whose `run` block loads a helper needing a provider,
// are reported by `tofu init` during TofuValidate.
//
// A direct module_source dep counts as used when its dir is --root, or when a
// checked file calls it by module_source or by a local path to one of its
// dirs.
//
// Unrecognised or malformed flags exit 2. Missing required flags, unreadable
// or invalid JSON or HCL, OpenTofu load errors, I/O errors or any failed check
// print rules_tofu: <error> to stderr and exit 1.
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
	"slices"
	"sort"
	"strings"

	"github.com/benbirt/rules_tofu/tf/private/cmd/internal/flags"
	"github.com/benbirt/rules_tofu/tf/private/cmd/internal/tfconfig"
	"github.com/benbirt/rules_tofu/tf/private/cmd/internal/tofushow"
	"github.com/hashicorp/hcl/v2"
	regaddr "github.com/opentofu/registry-address/v2"
)

var (
	tofu     = flag.String("tofu", "", "path to the tofu binary")
	workTree = flag.String("work-tree", "", "path to the work tree root")
	root     = flag.String("root", "", "work-tree-relative directory of the root module; empty for the workspace root package")
	modules  = flag.String("modules", "", "JSON file with modules, own_srcs, own_data and dep_dirs (see the package doc)")
	label    = flag.String("label", "", "the target being built, for error messages")
	out      = flag.String("out", "", "path of the modules.json to write")
	listed   = flags.StringList("direct-provider", "canonical address of a provider in the target's providers attribute (repeatable)")
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
		"--tofu":      *tofu,
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
		Modules []dep    `json:"modules"`
		OwnSrcs []string `json:"own_srcs"`
		OwnData []string `json:"own_data"`
		DepDirs []string `json:"dep_dirs"`
	}
	if err := json.Unmarshal(data, &listing); err != nil {
		return fmt.Errorf("parse %s: %w", *modules, err)
	}
	manifest, err := buildManifest(tofushow.Reader{Tofu: *tofu}, *workTree, *root, *label, listing.Modules, *testDir,
		own{srcs: listing.OwnSrcs, data: listing.OwnData, providers: *listed, depDirs: listing.DepDirs})
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
	// Dirs lists the work-tree directories of the library's srcs and data.
	Dirs []string `json:"dirs"`
	// Direct reports whether the target lists this library in deps. A direct
	// dep must be used.
	Direct bool `json:"direct"`
	// Callable reports whether checked files may call this library. It is true
	// for direct deps and for the library being built.
	Callable bool `json:"callable"`
}

// own holds the target's files and direct declarations for strict-deps checks.
type own struct {
	// srcs and data are work-tree paths of the target's srcs and data.
	srcs, data []string
	// providers holds canonical addresses from --direct-provider.
	providers []string
	// depDirs holds work-tree directories of direct deps' srcs and data.
	depDirs []string
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

// requirement is a non-built-in provider required by the target's srcs and
// absent from own.providers.
type requirement struct {
	// dir is the work-tree directory of the files that require it.
	dir      string
	provider string
}

// buildManifest constructs the manifest for the root module at root in
// workTree. testDir is "" for a rule with no test files, or the test
// directory relative to root, with "." meaning root itself. When testDir is
// non-empty, each `run` block's module in test files in root and in testDir
// is recorded as `tofu init` records it and walked like a call from root.
// own supplies the target's files and direct declarations for strict-deps
// checks.
func buildManifest(reader tofushow.Reader, workTree, root, label string, deps []dep, testDir string, own own) (*Manifest, error) {
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
	listed := map[string]bool{}
	for _, p := range own.providers {
		listed[p] = true
	}
	// directDir holds work-tree directories that checked files may call by
	// relative path. It combines the target's srcs and data directories with
	// depDirs.
	directDir := map[string]bool{}
	for _, dir := range own.depDirs {
		directDir[dir] = true
	}
	// mine holds absolute paths of the target's srcs and data.
	mine := map[string]bool{}
	for _, f := range append(append([]string{}, own.srcs...), own.data...) {
		directDir[dirOf(f)] = true
		mine[filepath.Join(workTree, filepath.FromSlash(f))] = true
	}
	// resolve checks a call to source from dir and returns the work-tree
	// directory it loads. For non-local calls it also returns the matching dep.
	// where names the call in errors.
	resolve := func(source, where, dir string) (string, *dep, error) {
		if tfconfig.IsLocal(source) {
			childDir := tfconfig.ResolveLocal(dir, source)
			if err := checkLocal(workTree, dir, childDir); err != nil {
				return "", nil, fmt.Errorf("%s: %s: source %q %w", label, where, source, err)
			}
			return childDir, nil, nil
		}
		d, subdir := lookup(all, source)
		if d == nil {
			return "", nil, fmt.Errorf("%s: %s: no dep provides source %q. %s", label, where, source, missingHint(source))
		}
		childDir := path.Join(d.Dir, subdir)
		if err := checkProvided(workTree, childDir, subdir, isGenerated(d)); err != nil {
			return "", nil, fmt.Errorf("%s: %s: %s (module_source %q) %w", label, where, hubLabel(d), d.Source, err)
		}
		if tfconfig.IsRegistry(source) && d.Version == "" {
			return "", nil, fmt.Errorf("%s: %s: %s provides the registry module %q, so it must set module_version", label, where, d.Label, d.Source)
		}
		return childDir, d, nil
	}
	// direct checks a call from one of the target's files against strict deps
	// and marks any direct dep it reaches as used.
	direct := func(source, where, dir string) error {
		childDir, d, err := resolve(source, where, dir)
		if err != nil {
			return err
		}
		if d != nil {
			if !d.Callable {
				return fmt.Errorf("%s: %s: source %q resolves to %q, which is not in the deps of %s; add %q to the deps of %s",
					label, where, source, hubLabel(d), label, hubLabel(d), label)
			}
			used[d.Label] = true
			return nil
		}
		if !directDir[childDir] {
			return fmt.Errorf("%s: %s: source %q resolves to the directory %q, which has no files from %s or from a tf_library in its deps; "+
				"add the tf_library whose files are in %q to the deps of %s", label, where, source, rootDir(childDir), label, rootDir(childDir), label)
		}
		for _, d := range all {
			if d.Direct && slices.Contains(d.Dirs, childDir) {
				used[d.Label] = true
			}
		}
		return nil
	}
	// active is the directory of each module on the path from the root to the
	// one being read, by its key. A call back into one of them is a cycle,
	// reported at once: tofu rejects it too, and short of one a module tree
	// is finite, so no depth limit is needed.
	active := map[string]string{}
	var walk func(key, dir string) error
	// visit records a call to source from dir under childKey and walks into
	// the module it loads. where names the call in errors.
	visit := func(source, childKey, where, dir string) error {
		record := Record{Key: childKey, Source: source}
		childDir, d, err := resolve(source, where, dir)
		if err != nil {
			return err
		}
		if d != nil && tfconfig.IsRegistry(source) {
			record.Version = d.Version
		}
		if above, ok := active[childDir]; ok {
			return fmt.Errorf("%s: %s: source %q is the directory of %s, an ancestor of this call, so the module calls form a cycle",
				label, where, source, moduleName(above))
		}
		rel, err := filepath.Rel(filepath.FromSlash(rootDir(root)), filepath.FromSlash(rootDir(childDir)))
		if err != nil {
			return err
		}
		record.Dir = filepath.ToSlash(rel)
		manifest.Modules = append(manifest.Modules, record)
		return walk(childKey, childDir)
	}
	// readDir asks OpenTofu about every configuration file in work-tree
	// directory dir across all targets, matching what `tofu init` loads, and
	// caches the result per directory.
	read := map[string]*tofushow.Module{}
	readDir := func(dir string) (*tofushow.Module, error) {
		if m, ok := read[dir]; ok {
			return m, nil
		}
		files, err := configFiles(workTree, dir)
		if err != nil {
			return nil, err
		}
		m, err := reader.ReadFiles(dir, files)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", label, err)
		}
		read[dir] = m
		return m, nil
	}
	walk = func(key, dir string) error {
		active[dir] = key
		defer delete(active, dir)
		m, err := readDir(dir)
		if err != nil {
			return err
		}
		for _, c := range m.Calls {
			childKey := c.Name
			if key != "" {
				childKey = key + "." + c.Name
			}
			if err := visit(c.Source, childKey, callIn(c.Name, dir), dir); err != nil {
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
		if err := walk("", root); err != nil {
			return nil, err
		}
	}
	if testDir != "" {
		runs, err := testRuns(workTree, root, testDir)
		if err != nil {
			return nil, err
		}
		// OpenTofu resolves a run block's module source from the root module's
		// directory. A helper module that calls back into root is a cycle.
		active[root] = ""
		for _, r := range runs {
			where := fmt.Sprintf("run %q (%s)", r.Name, displayRange(workTree, r.Range))
			// Test files from the module under test are not in mine.
			if mine[r.Range.Filename] {
				if err := direct(r.Source, where, root); err != nil {
					return nil, err
				}
			}
			if err := visit(r.Source, r.key, where, root); err != nil {
				return nil, err
			}
		}
		delete(active, root)
	}

	// Check the target's own configuration files directory by directory (no
	// rule allows configuration in data). ReadFiles stages only this target's
	// files in each directory, so every call and provider OpenTofu reports
	// belongs to this target (issue #153).
	byDir := map[string][]string{}
	for _, f := range own.srcs {
		if tfconfig.IsConfigFile(path.Base(f)) {
			dir := dirOf(f)
			byDir[dir] = append(byDir[dir], filepath.Join(workTree, filepath.FromSlash(f)))
		}
	}
	dirs := make([]string, 0, len(byDir))
	for dir := range byDir {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	var missing []requirement
	for _, dir := range dirs {
		m, err := reader.ReadFiles(dir, byDir[dir])
		if err != nil {
			return nil, fmt.Errorf("%s: %w%s", label, err, standAloneHint(label, err, byDir[dir]))
		}
		for _, p := range m.Providers {
			if !listed[p.Source] && !isBuiltin(p.Source) {
				missing = append(missing, requirement{dir: dir, provider: p.Source})
			}
		}
		for _, c := range m.Calls {
			if err := direct(c.Source, callIn(c.Name, dir), dir); err != nil {
				return nil, err
			}
		}
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

// dirOf returns the work-tree directory of work-tree path f, or "" at the
// root.
func dirOf(f string) string {
	if dir := path.Dir(f); dir != "." {
		return dir
	}
	return ""
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

// missingProviders formats the error for providers that label's srcs require
// and its providers attribute lacks. It names each
// @tf_providers//<namespace>/<name> label to add, or reports that rules_tofu
// cannot vendor providers outside registry.opentofu.org.
func missingProviders(label string, missing []requirement) error {
	var lines, labels, foreign []string
	seen := map[string]bool{}
	for _, m := range missing {
		lines = append(lines, fmt.Sprintf("    %s, required in %s", displayProvider(m.provider), displayDir(m.dir)))
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
	return fmt.Errorf("%s: its files require these providers, which are not in its providers:\n%s\n%s",
		label, strings.Join(lines, "\n"), strings.Join(advice, " "))
}

// defaultProviderHost is the registry rules_tofu vendors providers from,
// which a provider address may leave out.
var defaultProviderHost = regaddr.DefaultProviderRegistryHost.String()

// isBuiltin reports whether canonical provider address provider is built into
// OpenTofu and needs no plugin.
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
// work tree at all. generated is whether tf_modules generated the dep: its
// library holds the configuration of the module's root, its modules/
// subdirectories, the directories it calls itself and those its tag names in
// subdirs, and of no other.
func checkProvided(workTree, dir, subdir string, generated bool) error {
	if tfconfig.HasConfig(filepath.Join(workTree, filepath.FromSlash(dir))) {
		return nil
	}
	if subdir == "" {
		return errors.New("has no .tf, .tf.json, .tofu or .tofu.json files")
	}
	// A generated library has every configured directory below modules/, so
	// only one outside it can be there upstream and missing here.
	if generated && subdir != "modules" && !strings.HasPrefix(subdir, "modules/") {
		return fmt.Errorf("has no configuration in %q. A fetched module's library leaves out directories outside modules/ "+
			"that the module does not call itself: check the path, or add %q to `subdirs` on its tf_modules.module tag", subdir, subdir)
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

// hubLabel returns the public BUILD label for d. Libraries generated by
// tf_modules have private canonical labels ending in :_rules_tofu_module.
// Registry modules map to @tf_modules//<registry path>. Git and archive
// modules live in repositories ending with +tf_modules_<name> and map to
// @tf_modules//<name>. Module names cannot contain '+' or '/'. Other labels
// are returned unchanged.
func hubLabel(d *dep) string {
	repo, _, _ := strings.Cut(d.Label, "//")
	if !isGenerated(d) {
		return d.Label
	}
	if s, err := tfconfig.ParseSource(d.Source); err == nil && s.Kind == tfconfig.KindRegistry {
		return "@tf_modules//" + registryPath(s)
	}
	if i := strings.LastIndex(repo, generatedRepoPrefix); i >= 0 {
		return "@tf_modules//" + repo[i+len(generatedRepoPrefix):]
	}
	return d.Label
}

// isGenerated is whether tf_modules generated the library d.
func isGenerated(d *dep) bool {
	_, target, ok := strings.Cut(d.Label, "//")
	return ok && strings.HasSuffix(target, ":"+generatedTarget)
}

// generatedRepoPrefix precedes a git or archive module's name in the canonical
// name of the repository tf_modules creates for it.
const generatedRepoPrefix = "+tf_modules_"

// generatedTarget is the target name tf_modules gives each generated
// tf_library.
const generatedTarget = "_rules_tofu_module"

// registryPath is the path of a registry module under @tf_modules.
func registryPath(s tfconfig.Source) string {
	p := strings.ToLower(s.Namespace + "/" + s.Name + "/" + s.System)
	if s.Host != tfconfig.DefaultRegistryHost {
		p = s.Host + "/" + p
	}
	return p
}

func missingHint(source string) string {
	base, _ := tfconfig.SplitSubdir(source)
	if s, err := tfconfig.ParseSource(source); err == nil && s.Kind == tfconfig.KindRegistry {
		label := registryPath(s)
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

// callIn formats module call name in work-tree directory dir for error
// messages. OpenTofu's -module JSON output includes no file or line numbers.
func callIn(name, dir string) string {
	return fmt.Sprintf("module %q in %s", name, displayDir(dir))
}

// displayDir formats work-tree directory dir for error messages.
func displayDir(dir string) string {
	if dir == "" {
		return "the workspace root directory"
	}
	return fmt.Sprintf("%q", dir)
}

// standAloneHint returns a hint to append when OpenTofu fails to load a
// target's configuration files in one directory on their own. It returns ""
// unless err is a *tofushow.LoadError and files includes an override file. The
// failure may have another cause, so OpenTofu's own diagnostics are kept
// intact.
func standAloneHint(label string, err error, files []string) string {
	var load *tofushow.LoadError
	if !errors.As(err, &load) || !slices.ContainsFunc(files, func(f string) bool { return tfconfig.IsOverride(filepath.Base(f)) }) {
		return ""
	}
	return fmt.Sprintf("\n\nOpenTofu read the files of %s in this directory without any other target's files. "+
		"An override file must be in the same target as the block it overrides.", label)
}

// configFiles returns the paths under workTree of all files with a
// configuration extension in dir. Dot-prefixed files and .tofu shadowing are
// left to OpenTofu.
func configFiles(workTree, dir string) ([]string, error) {
	abs := filepath.Join(workTree, filepath.FromSlash(dir))
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && tfconfig.IsConfigFile(e.Name()) {
			files = append(files, filepath.Join(abs, e.Name()))
		}
	}
	return files, nil
}
