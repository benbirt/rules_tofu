// Package tfconfig reads the parts of an OpenTofu configuration that
// rules_tofu needs before tofu itself runs: module calls and the providers a
// module requires, and how module source addresses compare.
package tfconfig

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	regaddr "github.com/opentofu/registry-address/v2"
)

// Call is a `module` block.
type Call struct {
	Name   string
	Source string
	Range  hcl.Range
}

var (
	moduleSchema    = &hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: "module", LabelNames: []string{"name"}}}}
	sourceSchema    = &hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "source"}}}
	terraformSchema = &hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: "terraform"}}}
	requiredSchema  = &hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: "required_providers"}}}
)

// configExt returns name's configuration file extension, or "" if it isn't a
// configuration file. Since OpenTofu 1.8, a `.tofu` or `.tofu.json` file
// shadows the `.tf` or `.tf.json` file of the same name (see Effective), so a
// module can ship OpenTofu-specific configuration beside Terraform-compatible
// files.
func configExt(name string) string {
	for _, ext := range []string{".tf.json", ".tofu.json", ".tf", ".tofu"} {
		if strings.HasSuffix(name, ext) {
			return ext
		}
	}
	return ""
}

// IsConfigFile reports whether name is a configuration file: a `.tf`,
// `.tf.json`, `.tofu` or `.tofu.json` file.
func IsConfigFile(name string) bool {
	return configExt(name) != ""
}

// Effective drops each `.tf` or `.tf.json` file of files that a `.tofu` or
// `.tofu.json` file of the same path shadows, as tofu ignores them. The order
// of the rest is kept.
func Effective(files []string) []string {
	present := map[string]bool{}
	for _, f := range files {
		present[f] = true
	}
	var kept []string
	for _, f := range files {
		ext := configExt(f)
		if ext == ".tf" || ext == ".tf.json" {
			tofu := strings.TrimSuffix(f, ext) + strings.Replace(ext, ".tf", ".tofu", 1)
			if present[tofu] {
				continue
			}
		}
		kept = append(kept, f)
	}
	return kept
}

// ConfigFiles lists the configuration files in dir that tofu reads, sorted
// by name: those with a configuration extension, less the ones Effective
// drops.
func ConfigFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && !ignored(e.Name()) && IsConfigFile(e.Name()) {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	return Effective(files), nil
}

// ignored reports whether tofu passes over a file of this name when it
// reads a directory: one that starts with a dot. (It passes over editors'
// `name~` and `#name#` files too, which no configuration or test file's
// name can be.)
func ignored(name string) bool {
	return strings.HasPrefix(name, ".")
}

// configFiles lists the configuration files tofu reads in dir: primary
// files, then `_override` files, which tofu merges over them.
func configFiles(dir string) (primary, overrides []string, err error) {
	files, err := ConfigFiles(dir)
	if err != nil {
		return nil, nil, err
	}
	primary, overrides = splitOverrides(files)
	return primary, overrides, nil
}

// splitOverrides separates the `_override` files of files, which tofu
// merges over the rest, from the primary ones. Each group keeps its order.
func splitOverrides(files []string) (primary, overrides []string) {
	for _, f := range files {
		if isOverride(filepath.Base(f)) {
			overrides = append(overrides, f)
		} else {
			primary = append(primary, f)
		}
	}
	return primary, overrides
}

// HasConfig reports whether dir holds any configuration file.
func HasConfig(dir string) bool {
	files, err := ConfigFiles(dir)
	return err == nil && len(files) > 0
}

func parse(parser *hclparse.Parser, file string) (*hcl.File, error) {
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
	return f, nil
}

// ReadCalls parses the module blocks of the configuration files in dir. An
// `_override` file's `source` replaces the one in the primary file, as tofu
// merges it.
func ReadCalls(dir string) ([]Call, error) {
	primary, overrides, err := configFiles(dir)
	if err != nil {
		return nil, err
	}
	return readCalls(primary, overrides)
}

// ReadCallsFrom parses the module blocks of files, which share a module
// directory, as ReadCalls does for a directory. A `.tf` or `.tf.json` file
// that a `.tofu` or `.tofu.json` file of files shadows is skipped, as tofu
// skips it (see Effective).
func ReadCallsFrom(files []string) ([]Call, error) {
	primary, overrides := splitOverrides(Effective(files))
	sort.Strings(primary)
	sort.Strings(overrides)
	return readCalls(primary, overrides)
}

func readCalls(primary, overrides []string) ([]Call, error) {
	parser := hclparse.NewParser()
	var calls []Call
	index := map[string]int{}
	for pass, group := range [][]string{primary, overrides} {
		isOverride := pass == 1
		for _, file := range group {
			found, err := parseCalls(parser, file)
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
					return nil, fmt.Errorf("%s: duplicate module call %q", c.Range, c.Name)
				default:
					index[c.Name] = len(calls)
					calls = append(calls, c)
				}
			}
		}
	}
	for _, c := range calls {
		if c.Source == "" {
			return nil, fmt.Errorf("%s: module %q has no source", c.Range, c.Name)
		}
	}
	return calls, nil
}

func parseCalls(parser *hclparse.Parser, file string) ([]Call, error) {
	f, err := parse(parser, file)
	if err != nil {
		return nil, err
	}
	content, _, diags := f.Body.PartialContent(moduleSchema)
	if diags.HasErrors() {
		return nil, diags
	}
	var calls []Call
	for _, block := range content.Blocks {
		attrs, _, diags := block.Body.PartialContent(sourceSchema)
		if diags.HasErrors() {
			return nil, diags
		}
		c := Call{Name: block.Labels[0], Range: block.DefRange}
		if attr, ok := attrs.Attributes["source"]; ok {
			source, ok := LiteralString(attr.Expr)
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

// Requirement is a required_providers entry.
type Requirement struct {
	// Source is the provider's address, normalised (NormalizeProvider). An
	// entry without one is `hashicorp/<local name>`, as tofu implies it.
	Source  string `json:"source"`
	Version string `json:"version,omitempty"`
}

// RequiredProviders returns the normalised `source` of every
// required_providers entry in dir's configuration files, sorted.
func RequiredProviders(dir string) ([]string, error) {
	primary, overrides, err := configFiles(dir)
	if err != nil {
		return nil, err
	}
	reqs, err := ReadRequiredProviders(append(primary, overrides...))
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, r := range reqs {
		seen[r.Source] = true
	}
	sources := make([]string, 0, len(seen))
	for s := range seen {
		sources = append(sources, s)
	}
	sort.Strings(sources)
	return sources, nil
}

// Declared is one required_providers entry as a file declares it.
type Declared struct {
	// Name is the entry's local name.
	Name string
	Requirement
	// Range is where the entry is, for messages.
	Range string
	// Override is whether an `_override` file declares it.
	Override bool
}

// ReadRequirements returns every required_providers entry in files, which
// share a directory: the primary files' in file name order, then the
// `_override` files', each file's in the order written. A `.tf` or `.tf.json`
// file that a `.tofu` or `.tofu.json` file of files shadows is skipped, as
// tofu skips it (see Effective). Nothing is merged and nothing rejected:
// files of one directory may belong to several root modules (two tf_deploys
// in a package), so two entries of one name needn't be a mistake.
// ReadRequiredProviders reads a single module's.
//
// An entry is `{ source = ..., version = ... }`, or the legacy bare version
// string. Other keys, like configuration_aliases, are references that can't
// be evaluated here and don't name a provider, so they're skipped.
func ReadRequirements(files []string) ([]Declared, error) {
	primary, overrides := splitOverrides(Effective(files))
	sort.Strings(primary)
	sort.Strings(overrides)

	parser := hclparse.NewParser()
	var declared []Declared
	for pass, group := range [][]string{primary, overrides} {
		for _, file := range group {
			f, err := parse(parser, file)
			if err != nil {
				return nil, err
			}
			content, _, diags := f.Body.PartialContent(terraformSchema)
			if diags.HasErrors() {
				return nil, diags
			}
			for _, tf := range content.Blocks {
				inner, _, diags := tf.Body.PartialContent(requiredSchema)
				if diags.HasErrors() {
					return nil, diags
				}
				for _, block := range inner.Blocks {
					attrs, diags := block.Body.JustAttributes()
					if diags.HasErrors() {
						return nil, diags
					}
					// A map, so put the entries back in the order written.
					names := make([]string, 0, len(attrs))
					for name := range attrs {
						names = append(names, name)
					}
					sort.Slice(names, func(i, j int) bool {
						return attrs[names[i]].Range.Start.Byte < attrs[names[j]].Range.Start.Byte
					})
					for _, name := range names {
						attr := attrs[name]
						req, err := readRequirement(name, attr.Expr)
						if err != nil {
							return nil, fmt.Errorf("%s: %w", attr.Range, err)
						}
						declared = append(declared, Declared{
							Name:        name,
							Requirement: req,
							Range:       attr.Range.String(),
							Override:    pass == 1,
						})
					}
				}
			}
		}
	}
	return declared, nil
}

// ReadRequiredProviders returns the required_providers entries of one
// module, whose configuration files are files, keyed by local name (see
// ReadRequirements for how files are read). An entry in an `_override` file
// replaces the entry of that name, as tofu merges it; two primary entries of
// one name are an error, as in tofu.
func ReadRequiredProviders(files []string) (map[string]Requirement, error) {
	declared, err := ReadRequirements(files)
	if err != nil {
		return nil, err
	}
	reqs := map[string]Requirement{}
	where := map[string]string{}
	for _, d := range declared {
		if prior, ok := where[d.Name]; ok && !d.Override {
			return nil, fmt.Errorf("%s: provider %q is already required in %s", d.Range, d.Name, prior)
		}
		where[d.Name] = d.Range
		reqs[d.Name] = d.Requirement
	}
	return reqs, nil
}

func readRequirement(name string, expr hcl.Expression) (Requirement, error) {
	source := "hashicorp/" + name
	var version string
	if pairs, diags := hcl.ExprMap(expr); diags.HasErrors() {
		v, ok := LiteralString(expr)
		if !ok {
			return Requirement{}, fmt.Errorf("required_providers entry %q is neither an object nor a version string", name)
		}
		version = v
	} else {
		for _, pair := range pairs {
			key, ok := LiteralString(pair.Key)
			if !ok || (key != "source" && key != "version") {
				continue
			}
			value, ok := LiteralString(pair.Value)
			if !ok {
				return Requirement{}, fmt.Errorf("`%s` of required_providers entry %q must be a literal string", key, name)
			}
			if key == "source" {
				source = value
			} else {
				version = value
			}
		}
	}
	normalized, err := NormalizeProvider(source)
	if err != nil {
		return Requirement{}, fmt.Errorf("provider %q: %w", name, err)
	}
	return Requirement{Source: normalized, Version: version}, nil
}

// NormalizeProvider gives a provider source address in the canonical form
// OpenTofu uses for it, `<host>/<namespace>/<name>` in lower case:
// `HashiCorp/aws` is `registry.opentofu.org/hashicorp/aws`.
func NormalizeProvider(source string) (string, error) {
	p, err := regaddr.ParseProviderSource(source)
	if err != nil {
		return "", err
	}
	return p.String(), nil
}

// LiteralString evaluates expr with no variables in scope, as a string. The
// context is empty rather than nil: with a nil one the JSON syntax returns a
// template such as "${path.module}/x" as the literal text, where the native
// syntax reports the unknown variable, and both must be rejected alike.
func LiteralString(expr hcl.Expression) (string, bool) {
	v, diags := expr.Value(&hcl.EvalContext{})
	if diags.HasErrors() || v.IsNull() || !v.IsKnown() || v.Type().FriendlyName() != "string" {
		return "", false
	}
	return v.AsString(), true
}

// ModuleDirs lists the directories that a module's library serves, relative
// to repo: the module's own (moduleDir, relative to repo, "" for its root),
// every directory below its `modules/` (the standard place for submodules)
// that holds configuration, each directory of called (relative to repo) that
// holds configuration, and every directory those reach with local `source`
// paths that stay inside repo. Examples and tests are left out unless
// something calls them: called is for the submodules outside `modules/` that
// callers reach with `//<subdir>`, which only the callers know. The result is
// sorted.
func ModuleDirs(repo, moduleDir string, called ...string) ([]string, error) {
	moduleDir = strings.Trim(moduleDir, "/")
	if !HasConfig(filepath.Join(repo, filepath.FromSlash(moduleDir))) {
		return nil, fmt.Errorf("%q holds no .tf, .tf.json, .tofu or .tofu.json files", "/"+moduleDir)
	}
	pending := []string{moduleDir}
	for _, dir := range called {
		dir = strings.Trim(dir, "/")
		// A called directory without configuration fails the caller's build,
		// which says so; it isn't ours to report.
		if HasConfig(filepath.Join(repo, filepath.FromSlash(dir))) {
			pending = append(pending, dir)
		}
	}
	submodules := filepath.Join(repo, filepath.FromSlash(path.Join(moduleDir, "modules")))
	err := filepath.WalkDir(submodules, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return filepath.SkipAll
			}
			return err
		}
		if d.IsDir() && HasConfig(p) {
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

	seen := map[string]bool{}
	for len(pending) > 0 {
		dir := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if seen[dir] {
			continue
		}
		seen[dir] = true
		calls, err := ReadCalls(filepath.Join(repo, filepath.FromSlash(dir)))
		if err != nil {
			return nil, err
		}
		for _, c := range calls {
			if !IsLocal(c.Source) {
				continue
			}
			// A path out of the repository fails in tofu; it isn't ours to report.
			if child := ResolveLocal(dir, c.Source); child != ".." && !strings.HasPrefix(child, "../") {
				pending = append(pending, child)
			}
		}
	}
	dirs := make([]string, 0, len(seen))
	for d := range seen {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	return dirs, nil
}

// isOverride reports whether a configuration file is an override file, which
// tofu merges over the others.
func isOverride(name string) bool {
	stem := strings.TrimSuffix(name, configExt(name))
	return stem == "override" || strings.HasSuffix(stem, "_override")
}
