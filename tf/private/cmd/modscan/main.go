// Command modscan reads module sources and fetched modules for tf_modules.
// The module extension and its repository rule can't run Bazel-built tools,
// so they run this one as compiled by //tf/private:host_tools.bzl.
//
//	modscan parse <source>...
//
// reads each module source address as OpenTofu does (tfconfig.ParseSource)
// and prints a JSON list holding, for each, its tfconfig.Source or an
// `error`. The extension uses it to tell registry, git and archive sources
// apart and to find where each is fetched from.
//
//	modscan [--module=<label>=<source>...] [--self=<label>] [--git-submodule=<path>...] <repository dir> <module dir>
//
// describes a fetched module, for the repository rule that generates its
// tf_library. <module dir> is relative to the repository ("" for its root),
// each --module is a declared tf_modules.module, its label and source,
// --self is the label of the one being described, and each --git-submodule
// is the path of a git submodule of the fetched commit. rules_tofu doesn't
// fetch those, so the module's own directory, or a call of its own, that
// leads into one is an error here, where it can be named, rather than a
// missing directory when a caller builds.
// The module's library serves the module and its submodules, which callers
// reach with `//<subdir>`, so modscan scans the directories
// tfconfig.ModuleDirs lists: the module directory, each directory below its
// `modules/` (the standard place for submodules) that holds configuration,
// every directory those reach with local `source` paths, and every
// directory of the module's own that they call by its address with
// `//<subdir>`. Examples and tests are left out unless something calls
// them. It prints, as JSON:
//
//   - providers: every required provider, as `<host>/<namespace>/<name>`;
//   - deps: the label of each other declared module that a call uses: the
//     most specific match (tfconfig.BestMatch), as the modules manifest
//     resolves the call. A call the module itself provides is satisfied
//     with no dep;
//   - undeclared: each call by address that no declared module provides,
//     where it is, and the tf_modules.module tag that would declare it.
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
)

var (
	declared = flags.StringList("module", "`<label>=<source>` of a declared module (repeatable)")
	self     = flag.String("self", "", "`label` of the declared module being described")
	gitlinks = flags.StringList("git-submodule", "`path` of a git submodule of the fetched commit, which was not fetched (repeatable)")
)

// Result is modscan's output.
type Result struct {
	Providers  []string `json:"providers"`
	Deps       []string `json:"deps"`
	Undeclared []Call   `json:"undeclared"`
}

// Call is a module call by address.
type Call struct {
	Source string `json:"source"`
	Where  string `json:"where"`
	// Tag is the tf_modules.module tag that would declare the module called,
	// for an undeclared call.
	Tag string `json:"tag,omitempty"`
}

// Module is a declared module.
type Module struct {
	Label  string
	Source string
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "parse" {
		parse(os.Args[2:])
		return
	}
	flag.Parse()
	if flag.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: modscan [--module=<label>=<source>...] [--self=<label>] [--git-submodule=<path>...] <repository dir> <module dir>")
		os.Exit(2)
	}
	var modules []Module
	for _, m := range *declared {
		label, source, ok := strings.Cut(m, "=")
		if !ok {
			fmt.Fprintf(os.Stderr, "--module=%s: expected <label>=<source>\n", m)
			os.Exit(2)
		}
		modules = append(modules, Module{Label: label, Source: source})
	}
	result, err := scan(flag.Arg(0), flag.Arg(1), modules, *self, *gitlinks)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// notFetched is what every error about a git submodule ends with.
const notFetched = "rules_tofu does not fetch git submodules (OpenTofu does)"

// scan describes the module at moduleDir of repo. modules are the declared
// modules, and self is the label of the one being described ("" if it isn't
// among them). submodules are the paths in repo of the git submodules that
// the fetch left out.
func scan(repo, moduleDir string, modules []Module, self string, submodules []string) (*Result, error) {
	moduleDir = strings.Trim(moduleDir, "/")
	var gitlinks []string
	for _, s := range submodules {
		if s = strings.Trim(s, "/"); s != "" {
			gitlinks = append(gitlinks, s)
		}
	}
	submodules = gitlinks
	// submodule is the git submodule that dir is, or is inside, or "".
	submodule := func(dir string) string {
		for _, s := range submodules {
			if dir == s || strings.HasPrefix(dir, s+"/") {
				return s
			}
		}
		return ""
	}
	if s := submodule(moduleDir); s != "" {
		return nil, fmt.Errorf("%q is in the git submodule %q of the repository, and %s", "/"+moduleDir, s, notFetched)
	}

	sources := make([]string, len(modules))
	for i, m := range modules {
		sources[i] = m.Source
	}
	// resolve finds the declared module that provides a call, as the modules
	// manifest will: the most specific.
	resolve := func(source string) (*Module, string) {
		i, subdir := tfconfig.BestMatch(sources, source)
		if i < 0 {
			return nil, ""
		}
		return &modules[i], subdir
	}

	// The directories of its own that the module calls by its address, as a
	// caller would: `<self>//<subdir>`. They are part of what the library
	// serves, like a directory a local path reaches, and may call others in
	// turn, so read until no call adds one. A directory is marked before it
	// is read, so calls that form a cycle, or name a directory with no
	// configuration, end the loop all the same.
	called := map[string]bool{}
	var providers map[string]bool
	var calls map[string]Call
	for {
		dirs, err := tfconfig.ModuleDirs(repo, moduleDir, sorted(called)...)
		if err != nil {
			// A local path that leads below a submodule's own directory,
			// which is all a checkout leaves of it, is a directory that
			// isn't there. Any other error is not about submodules.
			var missing *fs.PathError
			if errors.As(err, &missing) {
				if rel, relErr := filepath.Rel(repo, missing.Path); relErr == nil {
					if s := submodule(filepath.ToSlash(rel)); s != "" {
						return nil, fmt.Errorf("%w (it is in the git submodule %q, and %s)", err, s, notFetched)
					}
				}
			}
			return nil, err
		}
		providers = map[string]bool{}
		calls = map[string]Call{}
		for _, dir := range dirs {
			abs := filepath.Join(repo, filepath.FromSlash(dir))
			required, err := tfconfig.RequiredProviders(abs)
			if err != nil {
				return nil, err
			}
			for _, p := range required {
				providers[p] = true
			}
			found, err := tfconfig.ReadCalls(abs)
			if err != nil {
				return nil, err
			}
			for _, c := range found {
				where := c.Range.Filename
				if rel, err := filepath.Rel(repo, where); err == nil {
					where = filepath.ToSlash(rel)
				}
				where = fmt.Sprintf("%s:%d", where, c.Range.Start.Line)
				if tfconfig.IsLocal(c.Source) {
					// The submodule's directory is there, and empty, so
					// ModuleDirs made nothing of the call.
					if s := submodule(tfconfig.ResolveLocal(dir, c.Source)); s != "" {
						return nil, fmt.Errorf("%s: module source %q leads into the git submodule %q, and %s", where, c.Source, s, notFetched)
					}
					continue
				}
				if _, ok := calls[c.Source]; !ok {
					calls[c.Source] = Call{Source: c.Source, Where: where}
				}
			}
		}
		grew := false
		for _, c := range calls {
			m, subdir := resolve(c.Source)
			if m == nil || m.Label != self {
				continue
			}
			dir := path.Join(moduleDir, subdir)
			if s := submodule(dir); s != "" {
				return nil, fmt.Errorf("%s: module source %q leads into the git submodule %q, and %s", c.Where, c.Source, s, notFetched)
			}
			if !called[dir] {
				called[dir] = true
				grew = true
			}
		}
		if !grew {
			break
		}
	}

	result := &Result{Providers: sorted(providers), Deps: []string{}, Undeclared: []Call{}}
	deps := map[string]bool{}
	for _, c := range calls {
		switch m, _ := resolve(c.Source); {
		case m == nil:
			c.Tag = tagFor(c.Source)
			result.Undeclared = append(result.Undeclared, c)
		case m.Label == self:
			// The module's own files: nothing to depend on, and a library
			// can't depend on itself.
		default:
			deps[m.Label] = true
		}
	}
	result.Deps = sorted(deps)
	sort.Slice(result.Undeclared, func(i, j int) bool { return result.Undeclared[i].Source < result.Undeclared[j].Source })
	return result, nil
}

// sorted is a set's members in order; never nil, so it encodes as `[]`.
func sorted(set map[string]bool) []string {
	members := make([]string, 0, len(set))
	for m := range set {
		members = append(members, m)
	}
	sort.Strings(members)
	return members
}

// Parsed is one source as `modscan parse` reports it.
type Parsed struct {
	tfconfig.Source
	Error string `json:"error,omitempty"`
}

func parse(sources []string) {
	parsed := make([]Parsed, len(sources))
	for i, s := range sources {
		src, err := tfconfig.ParseSource(s)
		parsed[i] = Parsed{Source: src}
		if err != nil {
			parsed[i].Error = err.Error()
		}
	}
	if err := json.NewEncoder(os.Stdout).Encode(parsed); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// tagFor is the tf_modules.module tag that declares the module a call to
// source needs: a registry module by its address without `//<subdir>`, any
// other by its source and a name to choose.
func tagFor(source string) string {
	if s, err := tfconfig.ParseSource(source); err == nil && s.Kind == tfconfig.KindRegistry {
		base, _ := tfconfig.SplitSubdir(source)
		return fmt.Sprintf(`tf_modules.module(source = %q, version = "...")`, base)
	}
	return fmt.Sprintf(`tf_modules.module(name = "...", source = %q)`, source)
}
