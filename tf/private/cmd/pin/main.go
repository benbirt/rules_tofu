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
//   - BAZEL: Bazel binary to run `bazel query` and `bazel build` with,
//     defaulting to bazel or bazelisk on PATH.
//
// Locking proceeds in order:
//   - One `bazel query` (`configQuery`, `queryFlags`) fetches every rule in
//     `deps()` of every tf_library, tf_deploy and tf_test in the main
//     repository across any repository, plus the configuration source files
//     among those dependencies. Output uses --output=streamed_jsonproto with
//     --proto:output_rule_attrs=srcs,actual,tf_modules_label,
//     --noproto:flatten_selects so every branch of a select() stays distinct
//     in selectorList instead of being dropped by Bazel's default flattening,
//     and --consistent_labels so labels print in canonical form and match
//     target labels in `bazel build` events. --keep_going and partial-result
//     exit code 3 are accepted because provider packages in @tf_providers may
//     not exist until locked. Bazel's package graph handles .bazelignore,
//     deleted packages and nested workspaces automatically. Files outside
//     Bazel rules are ignored, and a fetched module's examples are in no target
//     and left out.
//   - For each _tf_library, tf_deploy_rule and _tf_test returned, pin follows
//     srcs through filegroup srcs and alias actual for every combination of
//     select() branches the target reaches: unconditional labels plus one
//     branch of each select(). select()s with the same condition set take the
//     same branch together, since Bazel chooses the same condition for both in
//     any configuration; select()s with different condition sets combine in
//     full Cartesian product, including combinations no configuration selects.
//     A single-branch select() counts as unconditional, and combinations of one
//     target that yield the same labels are kept once. In each combination, pin
//     collects configuration source files and any labels that require building:
//     a declared output of a queried rule with a configuration extension (.tf,
//     .tofu, .tf.json, .tofu.json), a rule declaring such an output, or a
//     non-filegroup, non-alias rule declaring no outputs. A source file in a
//     package Bazel could not load is skipped because `bazel query` reports
//     that package itself; query files in no target's srcs are ignored.
//   - When any combination needs generated configuration, pin runs one
//     `bazel build --keep_going --remote_download_outputs=toplevel
//     --build_event_json_file=<temp> -- <labels>` in the workspace directory,
//     using the configuration of a plain `bazel build` there rather than flags
//     passed to `bazel run`. --remote_download_outputs=toplevel ensures
//     remote-cached outputs are on disk. From the build events, pin takes each
//     built label's default-output configuration files: their path under the
//     execution root and their work-tree directory under the output directory
//     (workspace-relative in the main repository, external/<repo>/<dir> in
//     external repositories). Exit code 1 (partial build under --keep_going) is
//     accepted: pin warns on each label that did not build and leaves providers
//     required only by it unlocked while locking the rest. When nothing needs
//     building, no build runs.
//   - Source files and built outputs are grouped by target, select() branch
//     combination and directory, matching what strict deps reads in a build
//     taking those branches. A file in two targets' srcs belongs to both
//     groups; duplicate groups of one target with the same directory and files
//     are dropped. Each group is read on its own with tofushow.Reader.ReadFiles
//     using the --tofu binary; identical directory and file sets across groups
//     are read once. Every provider OpenTofu reports counts: required_providers
//     entries after override files and .tofu shadowing, and providers implied
//     by a resource, data source or provider block. Implied providers have no
//     required_providers entry and therefore carry no version constraint.
//     Built-in providers are skipped via tfconfig.IsBuiltInProvider. If OpenTofu
//     cannot load a group, pin fails with OpenTofu's diagnostics and leaves the
//     lock unchanged. The error names a tf_modules library by its
//     tf_modules_label (@tf_modules//<path>), includes the chosen select()
//     conditions when the target reaches any, and appends
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
// `bazel query` fails other than with partial-result exit code 3, when
// `bazel build` fails other than with build-failure exit code 1, or when
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
	"maps"
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

// bazelCommand is the Bazel binary to run `query` and `build` with: $BAZEL,
// else bazel or bazelisk from PATH.
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
				return nil, false, fmt.Errorf("%s: %w%s", g.name(), err, tfconfig.StandAloneHint(g.name(), err, g.Files))
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

// group holds one target's configuration files in one directory for one
// combination of select() branches, matching what strict deps reads when Bazel
// builds that target under those conditions.
type group struct {
	// Label is the target's normalised canonical label (see mainLabel), or its
	// tf_modules_label (@tf_modules//<path>) for a library generated by
	// tf_modules.
	Label string
	// Branches lists the chosen condition of each select() the target reaches
	// (see selection.Branches), or nil when it reaches none.
	Branches []string
	// Dir is the work-tree directory: workspace-relative ("" for the workspace
	// root) in the main repository, or external/<repo>/<dir> in an external
	// repository.
	Dir string
	// Files holds sorted absolute paths on disk (source files and built
	// outputs).
	Files []string
}

// name formats the group's target for error messages, appending the chosen
// select() conditions when the target reaches any select().
func (g group) name() string {
	if len(g.Branches) == 0 {
		return g.Label
	}
	return fmt.Sprintf("%s (select() %s)", g.Label, strings.Join(g.Branches, ", "))
}

// queryGroups queries Bazel for the workspace's tf_library, tf_deploy and
// tf_test targets and any they depend on in other repositories. It returns the
// configuration files in each target's srcs, grouped by target, combination of
// select() branches and directory, following filegroup and alias targets as
// Bazel does.
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
// `bazel query` knows only source files. When srcs include generated
// configuration, queryGroups runs one `bazel build` for those labels (see
// build), merges the built outputs into their groups, and prints a warning to
// log for each label that did not build.
func queryGroups(bazel, workspace string, log io.Writer) ([]group, error) {
	out, err := query(bazel, workspace, configQuery, log)
	if err != nil {
		return nil, err
	}
	g, err := parseTargets(out)
	if err != nil {
		return nil, err
	}
	sels := g.selections()
	var built map[string][]sourceFile
	if labels := toBuild(sels); len(labels) > 0 {
		built, err = build(bazel, workspace, labels, log)
		if err != nil {
			return nil, err
		}
		for _, label := range labels {
			if _, ok := built[label]; !ok {
				fmt.Fprintf(log, "rules_tofu: pin could not build %s (errors above); providers required only by its configuration are not locked. "+
					"If the errors above name @tf_providers packages, running pin again after this run may build it; otherwise fix the failure and re-run pin.\n", label)
			}
		}
	}
	return g.groups(sels, built), nil
}

// buildFlags allow partial build results under --keep_going and ensure
// requested outputs cached remotely are downloaded to disk so pin can read
// them.
var buildFlags = []string{"--keep_going", "--remote_download_outputs=toplevel"}

// buildFailedExitCode is Bazel's exit code for a failed build, including a
// --keep_going build where only some targets succeeded.
const buildFailedExitCode = 1

// build runs one `bazel build` on labels in workspace, using the configuration
// of a plain `bazel build` there (from the workspace's .bazelrc, not flags
// passed to `bazel run`). It accepts exit code buildFailedExitCode and returns
// the configuration files among each built label's default outputs, keyed by
// normalised canonical label; labels that failed to build are omitted.
// Building a generated file or its rule analyses only that rule and its
// dependencies, not the tf targets listing it, so their providers need not be
// locked yet. A generator that itself depends on a tf target fails analysis
// while that target's providers are unlocked and builds on a subsequent pin
// run.
func build(bazel, workspace string, labels []string, log io.Writer) (map[string][]sourceFile, error) {
	dir, err := os.MkdirTemp("", "rules_tofu-pin-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	events := filepath.Join(dir, "build_events.json")
	args := append(append([]string{"build"}, buildFlags...), "--build_event_json_file="+events, "--")
	cmd := exec.Command(bazel, append(args, labels...)...)
	cmd.Dir = workspace
	cmd.Stdout = log
	cmd.Stderr = log
	err = cmd.Run()
	var exitErr *exec.ExitError
	if err != nil && !(errors.As(err, &exitErr) && exitErr.ExitCode() == buildFailedExitCode) {
		return nil, fmt.Errorf("bazel build: %w", err)
	}
	data, err := os.ReadFile(events)
	if err != nil {
		return nil, fmt.Errorf("bazel build: %w", err)
	}
	return parseBuildEvents(data)
}

// buildEvent holds the fields pin reads from a Build Event Protocol JSON event
// written by --build_event_json_file.
type buildEvent struct {
	ID struct {
		TargetCompleted *struct {
			Label string `json:"label"`
			// Aspect is non-empty when the event reports an aspect applied to
			// the target (for example from `build --aspects` in .bazelrc);
			// parseBuildEvents ignores such events so an aspect's completion
			// never replaces or stands in for its target's.
			Aspect string `json:"aspect"`
		} `json:"targetCompleted"`
		NamedSet *struct {
			ID string `json:"id"`
		} `json:"namedSet"`
	} `json:"id"`
	WorkspaceInfo *struct {
		LocalExecRoot string `json:"localExecRoot"`
	} `json:"workspaceInfo"`
	NamedSetOfFiles *struct {
		Files []struct {
			Name       string   `json:"name"`
			PathPrefix []string `json:"pathPrefix"`
		} `json:"files"`
		FileSets []struct {
			ID string `json:"id"`
		} `json:"fileSets"`
	} `json:"namedSetOfFiles"`
	Completed *struct {
		Success     bool `json:"success"`
		OutputGroup []struct {
			Name     string `json:"name"`
			FileSets []struct {
				ID string `json:"id"`
			} `json:"fileSets"`
		} `json:"outputGroup"`
	} `json:"completed"`
}

// parseBuildEvents decodes the Build Event Protocol JSON stream written by
// `bazel build --build_event_json_file`. For each target that completed
// successfully (ignoring aspect completions where targetCompleted.aspect is
// set), it expands nested namedSetOfFiles entries in the "default" output
// group and returns the configuration files keyed by normalised target label
// (see mainLabel). Each sourceFile's Dir is its directory under the output
// directory (matching group.Dir), and its Path is its absolute path under
// workspaceInfo.localExecRoot.
func parseBuildEvents(data []byte) (map[string][]sourceFile, error) {
	var execRoot string
	sets := map[string][]sourceFile{}
	nested := map[string][]string{}
	defaults := map[string][]string{}
	dec := json.NewDecoder(bytes.NewReader(data))
	for dec.More() {
		var e buildEvent
		if err := dec.Decode(&e); err != nil {
			return nil, fmt.Errorf("bazel build events: %w", err)
		}
		switch {
		case e.WorkspaceInfo != nil:
			execRoot = e.WorkspaceInfo.LocalExecRoot
		case e.ID.NamedSet != nil && e.NamedSetOfFiles != nil:
			id := e.ID.NamedSet.ID
			sets[id] = []sourceFile{}
			for _, f := range e.NamedSetOfFiles.Files {
				if !tfconfig.IsConfigFile(path.Base(f.Name)) {
					continue
				}
				dir := path.Dir(f.Name)
				if dir == "." {
					dir = ""
				}
				sets[id] = append(sets[id], sourceFile{Dir: dir, Path: path.Join(append(slices.Clone(f.PathPrefix), f.Name)...)})
			}
			for _, s := range e.NamedSetOfFiles.FileSets {
				nested[id] = append(nested[id], s.ID)
			}
		case e.ID.TargetCompleted != nil && e.ID.TargetCompleted.Aspect == "" && e.Completed != nil && e.Completed.Success:
			label := mainLabel(e.ID.TargetCompleted.Label)
			defaults[label] = []string{}
			for _, g := range e.Completed.OutputGroup {
				if g.Name == "default" {
					for _, s := range g.FileSets {
						defaults[label] = append(defaults[label], s.ID)
					}
				}
			}
		}
	}
	if execRoot == "" && len(defaults) > 0 {
		return nil, errors.New("bazel build events: no execution root")
	}
	built := map[string][]sourceFile{}
	for label, ids := range defaults {
		built[label] = []sourceFile{}
		seen := map[string]bool{}
		for len(ids) > 0 {
			id := ids[0]
			ids = ids[1:]
			if seen[id] {
				continue
			}
			seen[id] = true
			for _, f := range sets[id] {
				built[label] = append(built[label], sourceFile{Dir: f.Dir, Path: filepath.Join(execRoot, filepath.FromSlash(f.Path))})
			}
			ids = append(ids, nested[id]...)
		}
	}
	return built, nil
}

// queryFlags run `bazel query` with partial results allowed, without implicit
// or tool dependencies, emitting one JSON object per target with only the rule
// attributes pin reads. --noproto:flatten_selects keeps every branch of a
// select() in selectorList: Bazel's default flattening silently drops the
// select() part of `srcs = [...] + select(...)` and the whole of
// `actual = select(...)` on an alias. --consistent_labels prints every label
// in canonical form (@@repo+//pkg:name, or @@//pkg:name for the main
// repository) so query labels match the canonical target labels in build
// events.
var queryFlags = []string{
	"--keep_going", "--noimplicit_deps", "--notool_deps",
	"--output=streamed_jsonproto", "--proto:output_rule_attrs=srcs,actual,tf_modules_label",
	"--noproto:flatten_selects", "--consistent_labels",
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
	// Label is the condition label of a selectorList entry, empty on a plain
	// attribute.
	Label           string   `json:"label"`
	StringValue     string   `json:"stringValue"`
	StringListValue []string `json:"stringListValue"`
}

// labels returns the labels in v normalised with mainLabel so they compare as
// strings.
func (v queryValue) labels() []string {
	var labels []string
	if v.StringValue != "" {
		labels = append(labels, mainLabel(v.StringValue))
	}
	for _, s := range v.StringListValue {
		labels = append(labels, mainLabel(s))
	}
	return labels
}

// graph holds the source files and rules returned by `bazel query`, keyed by
// label.
type graph struct {
	files map[string]sourceFile
	rules map[string]rule
}

// sourceFile holds a configuration file's work-tree directory (as in
// group.Dir) and its absolute path on disk, for both source files and built
// outputs.
type sourceFile struct {
	Dir, Path string
}

// rule holds the fields pin reads from a Bazel rule: its class, its srcs and
// actual attributes with each select() preserved as its own element, its
// declared outputs, and tf_modules_label if set.
type rule struct {
	Class   string
	Srcs    []element
	Actual  []element
	Outputs []string
	// ModulesLabel is the @tf_modules//<path> label of a library generated by
	// tf_modules, or "".
	ModulesLabel string
}

// element is one operand of an attribute value: an unconditional value (one
// branch) or a select() with one branch per condition. For example,
// `["a.tf"] + select({...})` produces two elements.
type element []branch

// branch holds the labels of one select() branch, or of a plain attribute
// value (where Condition is "").
type branch struct {
	Condition string
	Labels    []string
}

// isSelect reports whether e has more than one branch. Plain values and
// single-branch select()s are treated as unconditional because they contribute
// the same labels in every configuration that builds.
func (e element) isSelect() bool {
	return len(e) > 1
}

// conditions returns the canonical key for e's condition set: its condition
// labels sorted and joined by NUL. Two select()s with the same condition set
// choose the same branch in any configuration, because Bazel resolves
// conditions identically for both.
func (e element) conditions() string {
	conds := make([]string, len(e))
	for i, b := range e {
		conds[i] = b.Condition
	}
	sort.Strings(conds)
	return strings.Join(conds, "\x00")
}

// pick returns the labels e contributes under choice, which maps each select()
// key (see conditions) to its chosen condition label. Unconditional elements
// return their sole branch.
func (e element) pick(choice map[string]string) []string {
	if !e.isSelect() {
		return e[0].Labels
	}
	cond := choice[e.conditions()]
	for _, b := range e {
		if b.Condition == cond {
			return b.Labels
		}
	}
	return nil
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
// --noproto:flatten_selects --consistent_labels` output, one JSON object per
// target, keeping the source files and rules.
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
				if a.Name == "tf_modules_label" {
					r.ModulesLabel = a.StringValue
					continue
				}
				if a.Name != "srcs" && a.Name != "actual" {
					continue
				}
				// With --noproto:flatten_selects, an attribute containing a
				// select() carries selectorList.elements: one element per
				// operand concatenated with `+` (plain values appear as
				// single-entry elements, select()s as one entry per branch).
				// Attributes without select() carry a plain queryValue.
				var elements []element
				if a.SelectorList != nil {
					for _, e := range a.SelectorList.Elements {
						var el element
						for _, v := range e.Entries {
							el = append(el, branch{Condition: mainLabel(v.Label), Labels: v.labels()})
						}
						if len(el) > 0 {
							elements = append(elements, el)
						}
					}
				} else if labels := a.labels(); len(labels) > 0 {
					elements = []element{{{Labels: labels}}}
				}
				if a.Name == "srcs" {
					r.Srcs = elements
				} else {
					r.Actual = elements
				}
			}
			g.rules[mainLabel(qt.Rule.Name)] = r
		}
	}
	return g, nil
}

// selection holds the source and buildable labels collected from one target's
// srcs for one combination of select() branches, before any build runs.
type selection struct {
	// Label is the target label, as in group.Label.
	Label string
	// Branches lists the chosen condition of each distinct select() condition
	// set the target reaches, ordered by sorted element.conditions key; nil
	// when it reaches no multi-branch select().
	Branches []string
	// Sources holds sorted labels of configuration source files.
	Sources []string
	// Built holds sorted labels pin must build for this combination (see
	// collect).
	Built []string
}

// selections returns, for every tf_library, tf_deploy and tf_test in g, the
// labels collected from its srcs (following filegroup srcs and alias actual)
// for each combination of select() branches it reaches. Results are ordered by
// queried target label, then by branch combination. select()s with the same
// condition set take the same branch together; others combine in full
// Cartesian product. Combinations of one target that yield the same Sources
// and Built labels are kept once.
func (g graph) selections() []selection {
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
	var sels []selection
	for _, label := range labels {
		r := g.rules[label]
		name := label
		if r.ModulesLabel != "" {
			name = r.ModulesLabel
		}
		keySet := map[string]bool{}
		g.selectKeys(r.Srcs, keySet, map[string]bool{})
		keys := slices.Sorted(maps.Keys(keySet))
		seen := map[string]bool{}
		for _, choice := range combinations(keys) {
			sources, built := map[string]bool{}, map[string]bool{}
			visited := map[string]bool{}
			for _, e := range r.Srcs {
				for _, src := range e.pick(choice) {
					g.collect(src, choice, outputs, sources, built, visited)
				}
			}
			sel := selection{Label: name, Sources: slices.Sorted(maps.Keys(sources)), Built: slices.Sorted(maps.Keys(built))}
			id := strings.Join(sel.Sources, "\x00") + "\x01" + strings.Join(sel.Built, "\x00")
			if seen[id] {
				continue
			}
			seen[id] = true
			for _, k := range keys {
				sel.Branches = append(sel.Branches, choice[k])
			}
			sels = append(sels, sel)
		}
	}
	return sels
}

// selectKeys adds to keys the element.conditions key of every multi-branch
// select() in elements and in any filegroup or alias reachable from any
// branch.
func (g graph) selectKeys(elements []element, keys, seen map[string]bool) {
	for _, e := range elements {
		if e.isSelect() {
			keys[e.conditions()] = true
		}
		for _, b := range e {
			for _, label := range b.Labels {
				if seen[label] {
					continue
				}
				seen[label] = true
				switch r := g.rules[label]; r.Class {
				case "filegroup":
					g.selectKeys(r.Srcs, keys, seen)
				case "alias":
					g.selectKeys(r.Actual, keys, seen)
				}
			}
		}
	}
}

// combinations returns the Cartesian product of choices across keys, each
// entry mapping each element.conditions key to one of its condition labels.
// When keys is empty, it returns one empty map.
func combinations(keys []string) []map[string]string {
	choices := []map[string]string{{}}
	for _, key := range keys {
		var next []map[string]string
		for _, choice := range choices {
			for _, cond := range strings.Split(key, "\x00") {
				c := maps.Clone(choice)
				c[key] = cond
				next = append(next, c)
			}
		}
		choices = next
	}
	return choices
}

// collect records what label contributes to srcs under choice: configuration
// source files go to sources, filegroup srcs and alias actual are followed
// recursively, and targets that require building go to built. A label goes to
// built if it is a declared output in outputs with a configuration extension,
// a rule declaring such an output, or a non-filegroup, non-alias rule that
// declares no outputs (since `bazel query` cannot name its outputs).
func (g graph) collect(label string, choice map[string]string, outputs, sources, built, seen map[string]bool) {
	if seen[label] {
		return
	}
	seen[label] = true
	if _, ok := g.files[label]; ok {
		sources[label] = true
		return
	}
	r, ok := g.rules[label]
	var next []element
	switch {
	case !ok:
		// Not a source file the query returned: a declared rule output, a file
		// without a configuration extension, or a file in a package Bazel
		// could not load, which the query reports itself. Only declared
		// configuration outputs are built.
		if outputs[label] && isConfigLabel(label) {
			built[label] = true
		}
	case r.Class == "filegroup":
		next = r.Srcs
	case r.Class == "alias":
		next = r.Actual
	case len(r.Outputs) == 0 || slices.ContainsFunc(r.Outputs, isConfigLabel):
		built[label] = true
	}
	for _, e := range next {
		for _, l := range e.pick(choice) {
			g.collect(l, choice, outputs, sources, built, seen)
		}
	}
}

// toBuild returns the sorted, deduplicated union of Built labels across sels.
func toBuild(sels []selection) []string {
	labels := map[string]bool{}
	for _, s := range sels {
		for _, l := range s.Built {
			labels[l] = true
		}
	}
	return slices.Sorted(maps.Keys(labels))
}

// groups turns sels into per-directory groups, ordered as sels and then by
// directory. built maps each built label to the configuration files it
// produced; labels absent from built contribute nothing. Within each directory,
// files are deduplicated by base name (keeping source paths before built
// outputs). For a given target, duplicate groups with the same directory and
// file set are dropped.
func (g graph) groups(sels []selection, built map[string][]sourceFile) []group {
	var groups []group
	seen := map[string]bool{}
	for _, s := range sels {
		// byDir keys each directory's files by base name, adding source paths
		// before built outputs. A rule with undeclared outputs can forward a
		// source file the target also lists, producing two paths to the same
		// file; two distinct files cannot share a directory and base name in a
		// build, and ReadFiles stages files by base name so a duplicate would
		// fail.
		byDir := map[string]map[string]string{}
		add := func(f sourceFile) {
			if byDir[f.Dir] == nil {
				byDir[f.Dir] = map[string]string{}
			}
			if _, ok := byDir[f.Dir][filepath.Base(f.Path)]; !ok {
				byDir[f.Dir][filepath.Base(f.Path)] = f.Path
			}
		}
		for _, label := range s.Sources {
			add(g.files[label])
		}
		for _, label := range s.Built {
			for _, f := range built[label] {
				add(f)
			}
		}
		for _, dir := range slices.Sorted(maps.Keys(byDir)) {
			files := slices.Sorted(maps.Values(byDir[dir]))
			id := s.Label + "\x00" + dir + "\x00" + strings.Join(files, "\x00")
			if seen[id] {
				continue
			}
			seen[id] = true
			groups = append(groups, group{Label: s.Label, Branches: s.Branches, Dir: dir, Files: files})
		}
	}
	return groups
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
