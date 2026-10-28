// Command modscan describes a fetched OpenTofu module for the repository rule
// of tf_modules.module, which generates the module's tf_library from it.
//
// The repository rule can't run Bazel-built tools, so it runs this one as
// compiled by //tf/private:host_tools.bzl. Usage:
//
//	modscan [--module=<label>=<source>...] <repository dir> <module dir>
//
// where <module dir> is relative to the repository ("" for its root), and
// each --module is a declared tf_modules.module: its label and source. The
// module's library serves the module and its submodules, which callers reach
// with `//<subdir>`, so modscan scans the module directory, each directory
// below its `modules/` (the standard place for submodules) that holds
// configuration, and every directory those reach with local `source` paths.
// Examples and tests are left out unless something calls them. It prints, as
// JSON:
//
//   - providers: every required provider, as `<host>/<namespace>/<name>`;
//   - deps: the label of each declared module that a call uses (see
//     tfconfig.Match; the longest source wins);
//   - undeclared: each call by address that no declared module provides, and
//     where it is.
package main

import (
	"encoding/json"
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

var declared = flags.StringList("module", "`<label>=<source>` of a declared module (repeatable)")

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
}

// Module is a declared module.
type Module struct {
	Label  string
	Source string
}

func main() {
	flag.Parse()
	if flag.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: modscan [--module=<label>=<source>...] <repository dir> <module dir>")
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
	result, err := scan(flag.Arg(0), flag.Arg(1), modules)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func scan(repo, moduleDir string, modules []Module) (*Result, error) {
	moduleDir = strings.Trim(moduleDir, "/")
	if !tfconfig.HasConfig(filepath.Join(repo, filepath.FromSlash(moduleDir))) {
		return nil, fmt.Errorf("%q holds no .tf or .tf.json files", "/"+moduleDir)
	}
	pending := []string{moduleDir}
	submodules := path.Join(moduleDir, "modules")
	err := filepath.WalkDir(filepath.Join(repo, filepath.FromSlash(submodules)), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return filepath.SkipAll
			}
			return err
		}
		if d.IsDir() && tfconfig.HasConfig(p) {
			rel, err := filepath.Rel(repo, p)
			if err != nil {
				return err
			}
			pending = append(pending, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	providers := map[string]bool{}
	calls := map[string]Call{}
	seen := map[string]bool{}
	for len(pending) > 0 {
		dir := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if dir == "." {
			dir = ""
		}
		if seen[dir] {
			continue
		}
		seen[dir] = true
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
			if tfconfig.IsLocal(c.Source) {
				child := path.Join(dir, c.Source)
				// A path out of the repository fails in tofu; it isn't ours to report.
				if child != ".." && !strings.HasPrefix(child, "../") {
					pending = append(pending, child)
				}
				continue
			}
			where := c.Range.Filename
			if rel, err := filepath.Rel(repo, where); err == nil {
				where = filepath.ToSlash(rel)
			}
			if _, ok := calls[c.Source]; !ok {
				calls[c.Source] = Call{Source: c.Source, Where: fmt.Sprintf("%s:%d", where, c.Range.Start.Line)}
			}
		}
	}

	result := &Result{Providers: []string{}, Deps: []string{}, Undeclared: []Call{}}
	for p := range providers {
		result.Providers = append(result.Providers, p)
	}
	sort.Strings(result.Providers)
	deps := map[string]bool{}
	for _, c := range calls {
		var best *Module
		for i := range modules {
			m := &modules[i]
			if _, ok := tfconfig.Match(m.Source, c.Source); ok && (best == nil || len(m.Source) > len(best.Source)) {
				best = m
			}
		}
		if best == nil {
			result.Undeclared = append(result.Undeclared, c)
		} else {
			deps[best.Label] = true
		}
	}
	for d := range deps {
		result.Deps = append(result.Deps, d)
	}
	sort.Strings(result.Deps)
	sort.Slice(result.Undeclared, func(i, j int) bool { return result.Undeclared[i].Source < result.Undeclared[j].Source })
	return result, nil
}
