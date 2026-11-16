// Command pin implements `bazel run @tf_providers//:pin`, rewriting the
// workspace's .terraform.lock.hcl for tf_providers.from_lock with
// `tofu providers lock`.
//
// Flags and environment:
//   - --tofu=<path>: path to the tofu binary.
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
//   - `bazel query` lists the .tf, .tf.json, .tofu and .tofu.json files of
//     every tf_library, tf_deploy and tf_test in the main repository and their
//     transitive tf_library deps. Bazel's package graph handles .bazelignore,
//     deleted packages and nested workspaces automatically. Any .tf file
//     outside Bazel rules, including a top-level versions.tf that no target
//     lists, is ignored. A fetched module's examples are in no target, so
//     they are left out.
//   - Only `terraform { required_providers { ... } }` blocks are extracted, so
//     resources, variables and module calls never need to evaluate out of
//     context.
//   - Within each directory, entries are combined by canonical provider
//     source rather than local name. Separate root modules in one package,
//     such as two tf_deploy targets with distinct srcs, each contribute all of
//     their constraints. Duplicate local names within one module are caught at
//     build time instead.
//   - Providers excluded via --exclude-source are dropped. When that leaves
//     nothing, pin writes an empty lock containing only tofu's header comment
//     without running tofu, so a provider moved from the lock to a tag stops
//     conflicting with it.
//   - Otherwise pin writes a scratch configuration in a temporary directory
//     with one child module per directory. OpenTofu combines the constraints
//     as it would for nested modules: exact pins in a versions.tf decide the
//     versions, and without one OpenTofu picks the newest version every
//     constraint accepts.
//   - `tofu get` and `tofu providers lock` run in that scratch directory with
//     no existing lock, since `tofu providers lock` keeps versions from an
//     existing lock and has no -upgrade flag. The resulting lock replaces
//     <workspace>/<lock-dir>/.terraform.lock.hcl.
//
// Progress and errors go to stderr. When `tofu providers lock` fails, OpenTofu
// does not report which module contributed each constraint, so pin prints
// every directory's constraints by provider. When `bazel query` fails other
// than with partial-result exit code 3, or when `tofu get` or
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

// module holds one directory's merged provider requirements, sorted by source
// (see requirements).
type module struct {
	Dir  string // workspace-relative with "" for the workspace root, or @<repo>//<dir>
	Reqs []tfconfig.Requirement
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
	excludedAny := false
	for _, dir := range dirs {
		reqs, err := requirements(byDir[dir])
		if err != nil {
			return err
		}
		kept := withoutSources(reqs, excludedSources)
		if len(kept) < len(reqs) {
			excludedAny = true
		}
		if len(kept) > 0 {
			modules = append(modules, module{Dir: dir, Reqs: kept})
		}
	}
	dst := filepath.Join(workspace, lockDir, ".terraform.lock.hcl")
	if len(modules) == 0 {
		if !excludedAny {
			return errors.New("no tf_library, tf_deploy or tf_test in the workspace declares required_providers to lock")
		}
		// Every requirement is on a provider the root module tags: the lock
		// has nothing to pin, and a stale entry left in it would conflict
		// with the tag because the extension fails that provider's fetch
		// until the lock no longer names it. Write tofu's header with no
		// providers.
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
	if err := os.WriteFile(dst, lock, 0o644); err != nil {
		return err
	}
	n := len(modules)
	fmt.Fprintf(log, "rules_tofu: wrote %s from the required_providers of %d director%s\n",
		dst, n, map[bool]string{true: "y", false: "ies"}[n == 1])
	return nil
}

// withoutSources drops the requirements on the given <namespace>/<name>
// sources. Sources compare as tofu does: case-insensitively, with the default
// registry host optional.
func withoutSources(reqs []tfconfig.Requirement, sources []string) []tfconfig.Requirement {
	drop := map[string]bool{}
	for _, s := range sources {
		if normalized, err := tfconfig.NormalizeProvider(s); err == nil {
			drop[normalized] = true
		}
	}
	var kept []tfconfig.Requirement
	for _, req := range reqs {
		if !drop[req.Source] {
			kept = append(kept, req)
		}
	}
	return kept
}

// requirements returns what one directory's files require: one Requirement per
// provider source, sorted by source, joining distinct version constraints with
// ", ", OpenTofu's AND operator.
//
// The files need not belong to one module: two tf_deploy targets in a package,
// each with its own srcs, may require providers under the same local name with
// the same or different sources (issue #99). Entries are therefore grouped by
// source rather than local name. Local names matter only for _override
// files, where an override entry replaces every earlier entry of its name.
func requirements(files []string) ([]tfconfig.Requirement, error) {
	declared, err := tfconfig.ReadRequirements(files)
	if err != nil {
		return nil, err
	}
	var effective []tfconfig.Declared
	for _, d := range declared {
		if d.Override {
			effective = slices.DeleteFunc(effective, func(e tfconfig.Declared) bool { return e.Name == d.Name })
		}
		effective = append(effective, d)
	}
	versions := map[string][]string{}
	for _, d := range effective {
		if _, ok := versions[d.Source]; !ok {
			versions[d.Source] = nil
		}
		// No version is no constraint: nothing to add to the others.
		if d.Version != "" && !slices.Contains(versions[d.Source], d.Version) {
			versions[d.Source] = append(versions[d.Source], d.Version)
		}
	}
	sources := make([]string, 0, len(versions))
	for s := range versions {
		sources = append(sources, s)
	}
	sort.Strings(sources)
	reqs := make([]tfconfig.Requirement, 0, len(sources))
	for _, s := range sources {
		sort.Strings(versions[s])
		reqs = append(reqs, tfconfig.Requirement{Source: s, Version: strings.Join(versions[s], ", ")})
	}
	return reqs, nil
}

// localNames gives each requirement in a scratch module a unique local name
// derived from the provider type, such as null for
// registry.opentofu.org/hashicorp/null, numbering collisions as aws-2 and so
// on. OpenTofu accepts only letters, digits and non-leading, non-trailing
// dashes in local names, so underscores are invalid; the lock file itself
// records provider sources rather than local names.
func localNames(reqs []tfconfig.Requirement) []string {
	names := make([]string, len(reqs))
	used := map[string]bool{}
	for i, req := range reqs {
		base := path.Base(req.Source)
		name := base
		for n := 2; used[name]; n++ {
			name = fmt.Sprintf("%s-%d", base, n)
		}
		used[name] = true
		names[i] = name
	}
	return names
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

// rootRules matches every tf_library, tf_deploy and tf_test in the main
// repository via their underlying rules _tf_library, tf_deploy_rule and
// _tf_test.
const rootRules = `kind("^(_tf_library|tf_deploy_rule|_tf_test) rule$", //...)`

// configQuery lists the configuration files of those rules, including those
// reached through filegroups and library deps, in any repository.
const configQuery = `filter("\.(tf|tofu)(\.json)?$", kind("source file", deps(` + rootRules + `)))`

// queryPartialExitCode is Bazel's exit code for a --keep_going command that
// produced results despite errors.
const queryPartialExitCode = 3

// queryConfigFiles asks Bazel for the configuration files of every tf_library,
// tf_deploy and tf_test in the main repository and their transitive tf_library
// deps. It returns absolute file paths grouped by directory:
// workspace-relative with "" for the workspace root in the main repository,
// and @<repo>//<dir> in external repositories.
//
// Every file counts, in any repository. A library's configuration files are
// all in its srcs, since no rule takes one as data (issue #140), and the
// library lists the providers they require: Bazel loads each of those labels
// with the library, whether or not anything calls the directory. A module
// that tf_modules fetched lists only the directories it serves, so its
// examples are not among them.
//
// The query runs with --keep_going because deps() also follows each rule's
// providers into @tf_providers, which cannot load before providers are locked:
// before the first pin @tf_providers//<ns>/<name> has no package, and an
// unusable lock entry makes its provider's repo fail when fetched. Neither has
// .tf files to find, and pin is how both are fixed.
func queryConfigFiles(bazel, workspace string, log io.Writer) (map[string][]string, error) {
	targets, err := query(bazel, workspace, configQuery, log)
	if err != nil {
		return nil, err
	}
	byDir := map[string][]string{}
	for _, f := range targets {
		dir := path.Dir(path.Join(f.Package, f.Name))
		if dir == "." {
			dir = ""
		}
		if f.Repo != "" {
			dir = "@" + f.Repo + "//" + dir
		}
		byDir[dir] = append(byDir[dir], f.Path)
	}
	return byDir, nil
}

// target is a source file from `bazel query --output=streamed_jsonproto`: its
// label split up, and its absolute path.
type target struct {
	Label, Repo, Package, Name, Path string
}

// queryFlags make the query print each target as a JSON object.
var queryFlags = []string{
	"--keep_going", "--noimplicit_deps", "--notool_deps",
	"--output=streamed_jsonproto",
}

// query runs a `bazel query`, accepting partial results.
func query(bazel, workspace, expr string, log io.Writer) ([]target, error) {
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
	return parseTargets(out)
}

// queryTarget holds the fields pin reads from a `bazel query
// --output=streamed_jsonproto` Target JSON object.
type queryTarget struct {
	Type       string `json:"type"`
	SourceFile *struct {
		Name     string `json:"name"`
		Location string `json:"location"`
	} `json:"sourceFile"`
}

// location is `<path>:<line>:<column>`.
var location = regexp.MustCompile(`^(.+):\d+:\d+$`)

// parseTargets reads `bazel query --output=streamed_jsonproto` output, one
// JSON object per target, keeping the source files. `Repo` is "" for the main
// repository, else the repository's name as Bazel prints it.
func parseTargets(out []byte) ([]target, error) {
	var targets []target
	dec := json.NewDecoder(bytes.NewReader(out))
	for dec.More() {
		var qt queryTarget
		if err := dec.Decode(&qt); err != nil {
			return nil, fmt.Errorf("bazel query output: %w", err)
		}
		if qt.Type != "SOURCE_FILE" || qt.SourceFile == nil {
			continue
		}
		t := target{Label: qt.SourceFile.Name, Path: qt.SourceFile.Location}
		if m := location.FindStringSubmatch(t.Path); m != nil {
			t.Path = m[1]
		}
		repo, rest, ok := strings.Cut(strings.TrimLeft(t.Label, "@"), "//")
		if !ok {
			continue
		}
		pkg, name, ok := strings.Cut(rest, ":")
		if !ok {
			continue
		}
		t.Repo, t.Package, t.Name = repo, pkg, name
		targets = append(targets, t)
	}
	return targets, nil
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
// combines them exactly as it would for nested modules. A child's local
// names are made up (see localNames).
func writeScratch(scratch string, children []module) error {
	rootDoc := map[string]any{}
	calls := map[string]any{}
	for i, name := range moduleNames(children) {
		dir := filepath.Join(scratch, "modules", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		required := map[string]tfconfig.Requirement{}
		for j, local := range localNames(children[i].Reqs) {
			required[local] = children[i].Reqs[j]
		}
		doc := map[string]any{"terraform": map[string]any{"required_providers": required}}
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
