// Command pin implements `bazel run @tf_providers//:pin`, rewriting the
// workspace's .terraform.lock.hcl for tf_providers.from_lock with
// `tofu providers lock`.
//
// Flags and environment:
//   - --tofu=<path>: path to the `tofu` binary, used both to read targets via
//     `tofu show` and to run `tofu providers lock`.
//   - --lock-dir=<dir>: workspace-relative directory of .terraform.lock.hcl,
//     or "" for the workspace root.
//   - --platform=<os>_<arch>: default platform passed as -platform= to
//     `tofu providers lock`, repeatable. Ignored when trailing arguments
//     already include -platform= or --platform=.
//   - --exclude-source=<source>: <namespace>/<name> or full provider address
//     pinned by a tf_providers.provider tag and left out of the lock,
//     repeatable.
//   - Trailing arguments after flags are forwarded to `tofu providers lock`.
//   - BUILD_WORKSPACE_DIRECTORY: workspace root set by `bazel run`, required.
//   - BAZEL: Bazel binary to run `bazel query` with, defaulting to bazel or
//     bazelisk on PATH.
//
// Locking proceeds in order:
//   - One `bazel query` (`configQuery`, `queryFlags`) fetches every rule in
//     `deps()` of every tf_library, tf_deploy and tf_test in the main
//     repository across any repository, plus the configuration source files
//     among those dependencies. Output uses --output=streamed_jsonproto with
//     --proto:output_rule_attrs=srcs,actual,tf_modules_label and
//     --noproto:flatten_selects so every branch of a select() appears in
//     selectorList instead of being dropped by Bazel's default flattening.
//     --keep_going and partial-result exit code 3 are accepted because provider
//     packages in @tf_providers may not exist until locked. Bazel's package
//     graph handles .bazelignore, deleted packages and nested workspaces
//     automatically. Files outside Bazel rules are ignored, and a fetched
//     module's examples are in no target and left out.
//   - For each _tf_library, tf_deploy_rule and _tf_test returned, pin collects
//     the configuration source files in its srcs, following filegroup srcs and
//     alias actual, and groups them by directory: workspace-relative in the
//     main repository, and external/<repo>/<dir> in external repositories. Each
//     group is one target's configuration files in one directory, matching what
//     strict deps reads in the build. A file in two targets' srcs belongs to
//     both groups. Query files in no such target's srcs are ignored. A select()
//     in srcs or in an alias's actual contributes every branch because
//     `bazel query` is unconfigured.
//   - Because `bazel query` knows only source files, pin cannot read generated
//     configuration in srcs. It prints a warning for each non-source target it
//     finds there: a srcs label that is a declared output of a queried rule
//     with a configuration extension, the declared configuration outputs of a
//     rule listed in srcs, or a non-filegroup, non-alias rule listed in srcs
//     that declares no outputs (warning on the rule label, since the query
//     cannot name its files). A source file in a package Bazel could not load
//     is not warned about here because `bazel query` reports that package
//     itself. Providers required only by generated files are not locked.
//   - Each group is read on its own with tofushow.Reader.ReadFiles using the
//     --tofu binary; groups with the same directory and files are read once.
//     Every provider OpenTofu reports counts: required_providers entries after
//     override files and .tofu shadowing, and providers implied by a resource,
//     data source or provider block. Implied providers have no
//     required_providers entry and therefore carry no version constraint.
//     Built-in providers are skipped via tfconfig.IsBuiltInProvider. If OpenTofu
//     cannot load a group, pin fails with OpenTofu's diagnostics and leaves the
//     lock unchanged. The error names a tf_modules library by its
//     tf_modules_label (@tf_modules//<path>) and appends
//     tfconfig.StandAloneHint when the group includes an override file.
//   - Providers excluded via --exclude-source are dropped. When that leaves no
//     providers and at least one was excluded, pin writes an empty lock
//     containing only `tofu`'s header comment without running
//     `tofu providers lock`, so a provider moved from the lock to a tag stops
//     conflicting with it. When no providers were required at all, pin fails.
//   - Otherwise pin combines constraints by canonical provider source rather
//     than local name (issue #99), within each directory for failure summaries
//     and across all directories for locking, and writes a scratch main.tf.json
//     with one root required_providers block (issue #151).
//   - `tofu providers lock` runs in that fresh scratch directory with no
//     existing lock, since it keeps versions from an existing lock and has no
//     -upgrade flag. The resulting lock replaces
//     <workspace>/<lock-dir>/.terraform.lock.hcl.
//
// Progress and errors go to stderr. When `tofu providers lock` fails, OpenTofu
// sees only the combined constraints, not which directory contributed each
// one, so pin prints each directory's combined constraints by provider, showing
// external repositories as external/<repo>/<dir>, unconstrained or implied
// providers as (any), and version constraints as OpenTofu normalises them. When
// `bazel query` fails other than with partial-result exit code 3, or when
// `tofu providers lock` exits non-zero, pin exits with that subprocess's exit
// code. Unrecognised or malformed flags exit 2, and other errors exit 1.
package main

import (
	"bytes"
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
	"slices"
	"sort"
	"strings"

	"github.com/benbirt/rules_tofu/tf/private/cmd/internal/flags"
	"github.com/benbirt/rules_tofu/tf/private/cmd/internal/tfconfig"
	"github.com/benbirt/rules_tofu/tf/private/cmd/internal/tofushow"
)

var (
	tofu      = flag.String("tofu", "", "path to the tofu binary")
	lockDir   = flag.String("lock-dir", "", "workspace-relative directory of the .terraform.lock.hcl; empty for the workspace root")
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

// emptyLock is the header-only .terraform.lock.hcl tofu writes when nothing is
// locked.
const emptyLock = "# This file is maintained automatically by \"tofu init\".\n" +
	"# Manual edits may be lost in future updates.\n"

// module holds the providers required in one directory, combined by source
// (see combine).
type module struct {
	Dir  string // workspace-relative with "" for the workspace root, or external/<repo>/<dir>
	Reqs []tofushow.Provider
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
	groups, err := queryGroups(bazel, workspace, log)
	if err != nil {
		return err
	}
	byDir, excludedAny, err := requirements(tofushow.Reader{Tofu: tofu}, groups, excludedSources)
	if err != nil {
		return err
	}
	dirs := make([]string, 0, len(byDir))
	for dir := range byDir {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	var modules []module
	var all []tofushow.Provider
	for _, dir := range dirs {
		modules = append(modules, module{Dir: dir, Reqs: combine(byDir[dir])})
		all = append(all, byDir[dir]...)
	}

	dst := filepath.Join(workspace, lockDir, ".terraform.lock.hcl")
	if len(modules) == 0 {
		if !excludedAny {
			return errors.New("no tf_library, tf_deploy or tf_test in the workspace requires a provider to lock")
		}
		// Every required provider is pinned by a root tag, so the lock has
		// nothing to pin. Leaving a stale entry in place would conflict with
		// the tag and make that provider's @tf_providers package fail to load.
		// Write tofu's header with no providers.
		if err := os.WriteFile(dst, []byte(emptyLock), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(log, "rules_tofu: every required provider is pinned by a tag in MODULE.bazel; wrote %s with no providers\n", dst)
		return nil
	}

	scratch, err := os.MkdirTemp("", "rules_tofu-pin-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	if err := writeScratch(scratch, combine(all)); err != nil {
		return err
	}

	args := []string{"-chdir=" + scratch, "providers", "lock"}
	if !hasPlatformArg(extraArgs) {
		for _, p := range defaultPlatforms {
			args = append(args, "-platform="+p)
		}
	}
	args = append(args, extraArgs...)

	if err := runTofu(tofu, args, log); err != nil {
		fmt.Fprint(log, constraintsSummary(modules))
		return fmt.Errorf("tofu providers lock: %w", err)
	}

	lock, err := os.ReadFile(filepath.Join(scratch, ".terraform.lock.hcl"))
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, lock, 0o644); err != nil {
		return err
	}
	n := len(modules)
	fmt.Fprintf(log, "rules_tofu: wrote %s for the providers required in %d director%s\n",
		dst, n, map[bool]string{true: "y", false: "ies"}[n == 1])
	return nil
}

// requirements asks OpenTofu which providers each group requires and returns
// them by directory, without built-in providers or those in excludedSources.
// excludedAny reports whether excludedSources dropped any.
//
// Each group is read on its own, as strict deps reads it in the build, so the
// providers found are the ones the build asks each target to list. Targets
// with the same files in a directory share one read.
func requirements(reader tofushow.Reader, groups []group, excludedSources []string) (byDir map[string][]tofushow.Provider, excludedAny bool, err error) {
	byDir = map[string][]tofushow.Provider{}
	read := map[string][]tofushow.Provider{}
	for _, g := range groups {
		key := g.Dir + "\x00" + strings.Join(g.Files, "\x00")
		providers, ok := read[key]
		if !ok {
			m, err := reader.ReadFiles(g.Dir, g.Files)
			if err != nil {
				return nil, false, fmt.Errorf("%s: %w%s", g.Label, err, tfconfig.StandAloneHint(g.Label, err, g.Files))
			}
			for _, p := range m.Providers {
				if !tfconfig.IsBuiltInProvider(p.Source) {
					providers = append(providers, p)
				}
			}
			read[key] = providers
		}
		kept := withoutSources(providers, excludedSources)
		if len(kept) < len(providers) {
			excludedAny = true
		}
		if len(kept) > 0 {
			byDir[g.Dir] = append(byDir[g.Dir], kept...)
		}
	}
	return byDir, excludedAny, nil
}

// withoutSources drops the providers with the given <namespace>/<name>
// sources. Sources compare as tofu does: case-insensitively, with the default
// registry host optional.
func withoutSources(providers []tofushow.Provider, sources []string) []tofushow.Provider {
	drop := map[string]bool{}
	for _, s := range sources {
		if normalized, err := tfconfig.NormalizeProvider(s); err == nil {
			drop[normalized] = true
		}
	}
	var kept []tofushow.Provider
	for _, p := range providers {
		if !drop[p.Source] {
			kept = append(kept, p)
		}
	}
	return kept
}

// combine returns one Provider per source, sorted by source. Distinct
// non-empty version constraints for a source are sorted and joined with ", ",
// which OpenTofu reads as requiring all of them. If every entry for a source
// has an empty Version, the combined Version is "".
//
// Providers are combined by source, never by local name: two targets, even in
// one directory, may give one local name to different providers (issue #99).
func combine(providers []tofushow.Provider) []tofushow.Provider {
	versions := map[string][]string{}
	for _, p := range providers {
		if _, ok := versions[p.Source]; !ok {
			versions[p.Source] = nil
		}
		// No version is no constraint: nothing to add to the others.
		if p.Version != "" && !slices.Contains(versions[p.Source], p.Version) {
			versions[p.Source] = append(versions[p.Source], p.Version)
		}
	}
	sources := make([]string, 0, len(versions))
	for s := range versions {
		sources = append(sources, s)
	}
	sort.Strings(sources)
	combined := make([]tofushow.Provider, 0, len(sources))
	for _, s := range sources {
		sort.Strings(versions[s])
		combined = append(combined, tofushow.Provider{Source: s, Version: strings.Join(versions[s], ", ")})
	}
	return combined
}

// localNames gives each provider in a scratch module a unique local name
// derived from the provider type, such as null for
// registry.opentofu.org/hashicorp/null, numbering collisions as aws-2 and so
// on. OpenTofu accepts only letters, digits and non-leading, non-trailing
// dashes in local names, so underscores are invalid; the lock file itself
// records provider sources rather than local names.
func localNames(providers []tofushow.Provider) []string {
	names := make([]string, len(providers))
	used := map[string]bool{}
	for i, p := range providers {
		base := path.Base(p.Source)
		name := base
		for n := 2; used[name]; n++ {
			name = fmt.Sprintf("%s-%d", base, n)
		}
		used[name] = true
		names[i] = name
	}
	return names
}

func runTofu(tofu string, args []string, log io.Writer) error {
	cmd := exec.Command(tofu, args...)
	cmd.Stdout = log
	cmd.Stderr = log
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

// tfRules are the rule classes behind tf_library, tf_deploy and tf_test.
var tfRules = []string{"_tf_library", "tf_deploy_rule", "_tf_test"}

// configQuery selects every tf_library, tf_deploy and tf_test in the main
// repository, all rules in their transitive dependencies across any
// repository, and the configuration source files among those dependencies.
const configQuery = `let d = deps(kind("^(_tf_library|tf_deploy_rule|_tf_test) rule$", //...)) in ` +
	`kind(" rule$", $d) + filter("\.(tf|tofu)(\.json)?$", kind("source file", $d))`

// queryPartialExitCode is Bazel's exit code for a --keep_going command that
// produced results despite errors.
const queryPartialExitCode = 3

// group holds the configuration source files that one target lists in one
// directory: what strict deps reads on its own in the build.
type group struct {
	// Label is the target, as Bazel prints it; a library generated by
	// tf_modules goes by its tf_modules_label (@tf_modules//<path>).
	Label string
	// Dir is workspace-relative with "" for the workspace root in the main
	// repository, and external/<repo>/<dir> in another, as in a work tree.
	Dir string
	// Files holds absolute paths, sorted.
	Files []string
}

// queryGroups queries Bazel for the workspace's tf_library, tf_deploy and
// tf_test targets and the tf_library targets they depend on in any repository.
// It returns the configuration source files in each target's srcs, grouped by
// target and directory, following filegroup and alias targets as Bazel does.
//
// A library lists the providers its srcs require, and Bazel loads each of
// those labels with the library, whether or not anything calls the directory.
// A module fetched by tf_modules lists only the directories it serves, so its
// examples are excluded.
//
// The query runs with --keep_going because deps() follows providers into
// @tf_providers: an unpinned provider has no @tf_providers//<ns>/<name>
// package yet, and an unusable lock entry or lock/tag conflict generates a
// package that fails to load. Neither holds configuration, and pin fixes both.
//
// `bazel query` knows only source files. For each generated configuration file
// in srcs, or any non-filegroup, non-alias rule in srcs that declares no
// outputs, queryGroups prints a warning to log.
func queryGroups(bazel, workspace string, log io.Writer) ([]group, error) {
	out, err := query(bazel, workspace, configQuery, log)
	if err != nil {
		return nil, err
	}
	g, err := parseTargets(out)
	if err != nil {
		return nil, err
	}
	groups, generated := g.groups()
	for _, label := range generated {
		fmt.Fprintf(log, "rules_tofu: pin cannot read %s, which is not a source file; providers required only by generated files are not locked\n", label)
	}
	return groups, nil
}

// queryFlags run `bazel query` with partial results allowed, without implicit
// or tool dependencies, emitting one JSON object per target with only the rule
// attributes pin reads. --noproto:flatten_selects keeps every branch of a
// select() in selectorList: Bazel's default flattening silently drops the
// select() part of `srcs = [...] + select(...)` and the whole of
// `actual = select(...)` on an alias.
var queryFlags = []string{
	"--keep_going", "--noimplicit_deps", "--notool_deps",
	"--output=streamed_jsonproto", "--proto:output_rule_attrs=srcs,actual,tf_modules_label",
	"--noproto:flatten_selects",
}

// query runs a `bazel query`, accepting partial results.
func query(bazel, workspace, expr string, log io.Writer) ([]byte, error) {
	cmd := exec.Command(bazel, append(append([]string{"query"}, queryFlags...), expr)...)
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
	return out, nil
}

// queryTarget holds the fields pin reads from a `bazel query
// --output=streamed_jsonproto` Target JSON object.
type queryTarget struct {
	Type       string `json:"type"`
	SourceFile *struct {
		Name     string `json:"name"`
		Location string `json:"location"`
	} `json:"sourceFile"`
	Rule *struct {
		Name      string `json:"name"`
		RuleClass string `json:"ruleClass"`
		Attribute []struct {
			Name string `json:"name"`
			queryValue
			SelectorList *struct {
				Elements []struct {
					Entries []queryValue `json:"entries"`
				} `json:"elements"`
			} `json:"selectorList"`
		} `json:"attribute"`
		RuleOutput []string `json:"ruleOutput"`
	} `json:"rule"`
}

// queryValue holds the string or string-list value of a plain attribute or of
// one entry in a selectorList.
type queryValue struct {
	StringValue     string   `json:"stringValue"`
	StringListValue []string `json:"stringListValue"`
}

// graph holds the source files and rules returned by `bazel query`, keyed by
// label.
type graph struct {
	files map[string]sourceFile
	rules map[string]rule
}

// sourceFile is a source file's directory, in the form of group.Dir, and its
// absolute path.
type sourceFile struct {
	Dir, Path string
}

// rule holds the fields pin reads from a Bazel rule: its class, the labels in
// its srcs and actual attributes across every branch of any select(), its
// declared outputs, and tf_modules_label if set.
type rule struct {
	Class   string
	Srcs    []string
	Actual  []string
	Outputs []string
	// ModulesLabel is the @tf_modules//<path> label of a library generated by
	// tf_modules, or "".
	ModulesLabel string
}

// location is `<path>:<line>:<column>`.
var location = regexp.MustCompile(`^(.+):\d+:\d+$`)

// mainLabel strips the leading @@ that Bazel prints on main-repository labels
// in some contexts, so labels compare as strings.
func mainLabel(label string) string {
	if strings.HasPrefix(label, "@@//") {
		return label[2:]
	}
	return label
}

// splitLabel splits a label into its repository name without leading `@`
// ("" for the main repository), package and target name.
func splitLabel(label string) (repo, pkg, name string, ok bool) {
	repo, rest, ok := strings.Cut(strings.TrimLeft(label, "@"), "//")
	if !ok {
		return "", "", "", false
	}
	pkg, name, ok = strings.Cut(rest, ":")
	return repo, pkg, name, ok
}

// parseTargets reads `bazel query --output=streamed_jsonproto
// --noproto:flatten_selects` output, one JSON object per target, keeping the
// source files and rules.
func parseTargets(out []byte) (graph, error) {
	g := graph{files: map[string]sourceFile{}, rules: map[string]rule{}}
	dec := json.NewDecoder(bytes.NewReader(out))
	for dec.More() {
		var qt queryTarget
		if err := dec.Decode(&qt); err != nil {
			return graph{}, fmt.Errorf("bazel query output: %w", err)
		}
		switch {
		case qt.Type == "SOURCE_FILE" && qt.SourceFile != nil:
			repo, pkg, name, ok := splitLabel(qt.SourceFile.Name)
			if !ok {
				continue
			}
			dir := path.Dir(path.Join(pkg, name))
			if dir == "." {
				dir = ""
			}
			if repo != "" {
				dir = path.Join("external", repo, dir)
			}
			file := qt.SourceFile.Location
			if m := location.FindStringSubmatch(file); m != nil {
				file = m[1]
			}
			g.files[mainLabel(qt.SourceFile.Name)] = sourceFile{Dir: dir, Path: file}
		case qt.Type == "RULE" && qt.Rule != nil:
			r := rule{Class: qt.Rule.RuleClass}
			for _, o := range qt.Rule.RuleOutput {
				r.Outputs = append(r.Outputs, mainLabel(o))
			}
			for _, a := range qt.Rule.Attribute {
				// With --noproto:flatten_selects, an attribute containing a
				// select() carries a selectorList whose elements are the plain
				// values and select()s concatenated together; other attributes
				// carry a plain value.
				values := []queryValue{a.queryValue}
				if a.SelectorList != nil {
					for _, e := range a.SelectorList.Elements {
						values = append(values, e.Entries...)
					}
				}
				for _, v := range values {
					switch a.Name {
					case "srcs":
						for _, s := range v.StringListValue {
							r.Srcs = append(r.Srcs, mainLabel(s))
						}
					case "actual":
						if v.StringValue != "" {
							r.Actual = append(r.Actual, mainLabel(v.StringValue))
						}
					case "tf_modules_label":
						r.ModulesLabel = v.StringValue
					}
				}
			}
			g.rules[mainLabel(qt.Rule.Name)] = r
		}
	}
	return g, nil
}

// groups returns the configuration source files of every tf_library, tf_deploy
// and tf_test in g, one group per target and directory, sorted by target label
// and directory. generated lists the sorted labels of generated configuration
// files in their srcs, and of any non-filegroup, non-alias rules in their srcs
// that declare no outputs.
func (g graph) groups() (groups []group, generated []string) {
	labels := make([]string, 0, len(g.rules))
	for label, r := range g.rules {
		if slices.Contains(tfRules, r.Class) {
			labels = append(labels, label)
		}
	}
	sort.Strings(labels)
	// A generated file is known by its label only if its rule declares it.
	outputs := map[string]bool{}
	for _, r := range g.rules {
		for _, o := range r.Outputs {
			outputs[o] = true
		}
	}
	notRead := map[string]bool{}
	for _, label := range labels {
		files := map[string]bool{}
		for _, src := range g.rules[label].Srcs {
			g.collect(src, outputs, files, notRead, map[string]bool{})
		}
		byDir := map[string][]string{}
		for f := range files {
			byDir[g.files[f].Dir] = append(byDir[g.files[f].Dir], g.files[f].Path)
		}
		dirs := make([]string, 0, len(byDir))
		for dir := range byDir {
			dirs = append(dirs, dir)
		}
		sort.Strings(dirs)
		name := label
		if hub := g.rules[label].ModulesLabel; hub != "" {
			name = hub
		}
		for _, dir := range dirs {
			sort.Strings(byDir[dir])
			groups = append(groups, group{Label: name, Dir: dir, Files: byDir[dir]})
		}
	}
	for label := range notRead {
		generated = append(generated, label)
	}
	sort.Strings(generated)
	return groups, generated
}

// collect adds to files the configuration source files that label contributes
// to a srcs attribute: the file itself, or the files of a filegroup or alias,
// followed as Bazel follows them. Non-source targets are recorded in
// generated: a label in outputs with a configuration extension, the declared
// configuration outputs of a rule, or a non-filegroup, non-alias rule that
// declares no outputs, whose files the query cannot name.
func (g graph) collect(label string, outputs, files, generated, seen map[string]bool) {
	if seen[label] {
		return
	}
	seen[label] = true
	if _, ok := g.files[label]; ok {
		files[label] = true
		return
	}
	r, ok := g.rules[label]
	switch {
	case !ok:
		// Not a source file the query returned: a declared rule output, a file
		// without a configuration extension, or a file in a package Bazel
		// could not load, which the query reports itself. Only declared
		// configuration outputs are recorded as generated.
		if outputs[label] && isConfigLabel(label) {
			generated[label] = true
		}
	case r.Class == "filegroup":
		for _, src := range r.Srcs {
			g.collect(src, outputs, files, generated, seen)
		}
	case r.Class == "alias":
		for _, actual := range r.Actual {
			g.collect(actual, outputs, files, generated, seen)
		}
	case len(r.Outputs) == 0:
		generated[label] = true
	default:
		for _, o := range r.Outputs {
			if isConfigLabel(o) {
				generated[o] = true
			}
		}
	}
}

// isConfigLabel reports whether label names a configuration file.
func isConfigLabel(label string) bool {
	_, _, name, ok := splitLabel(label)
	return ok && tfconfig.IsConfigFile(name)
}

// writeScratch writes main.tf.json in scratch for `tofu providers lock`: a
// root module containing only a required_providers block, with local names
// from localNames.
func writeScratch(scratch string, providers []tofushow.Provider) error {
	required := map[string]map[string]string{}
	for i, local := range localNames(providers) {
		entry := map[string]string{"source": providers[i].Source}
		if providers[i].Version != "" {
			entry["version"] = providers[i].Version
		}
		required[local] = entry
	}
	doc := map[string]any{"terraform": map[string]any{"required_providers": required}}
	return writeJSON(filepath.Join(scratch, "main.tf.json"), doc)
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
	for _, m := range modules {
		dir := m.Dir
		if dir == "" {
			dir = "(workspace root)"
		}
		for _, req := range m.Reqs {
			version := req.Version
			if version == "" {
				version = "(any)"
			}
			bySource[req.Source] = append(bySource[req.Source], entry{dir, version})
		}
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
