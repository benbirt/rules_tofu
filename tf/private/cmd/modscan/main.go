// Command modscan reads module source addresses and fetched module trees for
// tf_modules. Module extensions and repository rules cannot run Bazel-built
// tools, so they compile and run modscan via //tf/private:host_tools.bzl.
//
//	modscan parse <source>...
//
// Parses each source address with tfconfig.ParseSource and writes a JSON
// array of Parsed objects to stdout. No flags are recognised, and parse
// errors are recorded in each element's error field rather than failing the
// command. The extension uses it to tell registry, git and archive sources
// apart and to find where each is fetched from.
//
//	modscan [--module=<label>=<source>...] [--self=<label>] [--git-submodule=<path>...] <repository dir> <module dir>
//
// Inspects a fetched module to generate its tf_library. <module dir> is
// relative to <repository dir>, or "" for its root, and must contain
// configuration files.
//   - --module=<label>=<source>: a declared tf_modules.module tag, repeatable.
//   - --self=<label>: label of the module being scanned, so calls to
//     `<self>//<subdir>` are scanned as internal submodules rather than deps.
//   - --git-submodule=<path>: repository-relative path of an unfetched git
//     submodule, repeatable. OpenTofu fetches git submodules and rules_tofu
//     does not. If <module dir> or any call from its served directories
//     enters one, modscan fails here naming the submodule rather than leaving
//     a caller's build to fail on a missing directory.
//
// modscan walks the directories returned by tfconfig.ModuleDirs together with
// any subdirectories reached by `<self>//<subdir>` calls, leaving uncalled
// examples and tests out. It writes a JSON Result to stdout:
//   - providers: sorted canonical `<host>/<namespace>/<name>` addresses of
//     required providers, or `[]` when none.
//   - deps: sorted labels of other declared modules that calls match via
//     tfconfig.BestMatch, as the modules manifest resolves each call, or `[]`
//     when none. Self-calls add no dep.
//   - undeclared: non-local calls that no declared module matches, sorted by
//     source, each with source, where and a suggested tf_modules.module tag,
//     or `[]` when none.
//
// Scan mode exits 2 on invalid flags or arguments and 1 on scan or I/O errors
// after printing the error to stderr.
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

// Result is the JSON object written by scan mode; slices are never nil.
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

// scan describes the module at moduleDir in repo, where "" is repo's root.
// modules lists the declared modules, and self is the label of the one being
// described, or "" if it is not among them. submodules lists the paths in
// repo of git submodules that the fetch left out.
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
	// A fetched module is declared by the directory callers call, which must
	// hold configuration. tfconfig.ModuleDirs itself allows an unconfigured
	// root because to pin such a library is a bundle of submodules.
	if !tfconfig.HasConfig(filepath.Join(repo, filepath.FromSlash(moduleDir))) {
		return nil, fmt.Errorf("%q holds no .tf, .tf.json, .tofu or .tofu.json files", "/"+moduleDir)
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

	// Directories of its own that the module calls by address with
	// `<self>//<subdir>` are part of what the library serves, like a
	// directory a local path reaches (issue #95). They may call others in
	// turn, so repeat until no call adds one. Marking each directory before
	// reading it ensures cycles and directories without configuration still
	// terminate the loop.
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
