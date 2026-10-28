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
)

// Call is a `module` block.
type Call struct {
	Name   string
	Source string
	Range  hcl.Range
}

// IsLocal reports whether tofu reads source as a local path.
func IsLocal(source string) bool {
	return strings.HasPrefix(source, "./") || strings.HasPrefix(source, "../")
}

// SplitSubdir splits off a source's `//<subdir>`, the way tofu does: the
// first `//` that isn't part of a `scheme://`. A query string (`?ref=`) stays
// with the base address.
func SplitSubdir(source string) (string, string) {
	rest := source
	offset := 0
	if i := strings.Index(rest, "://"); i >= 0 {
		offset = i + 3
		rest = rest[offset:]
	}
	i := strings.Index(rest, "//")
	if i < 0 {
		return source, ""
	}
	base := source[:offset+i]
	subdir := rest[i+2:]
	if q := strings.Index(subdir, "?"); q >= 0 {
		base += subdir[q:]
		subdir = subdir[:q]
	}
	return base, strings.Trim(subdir, "/")
}

// DefaultRegistryHost is the registry a module or provider address may leave
// out.
const DefaultRegistryHost = "registry.opentofu.org"

// IsRegistry reports whether tofu reads source as a module registry address:
// `[<host>/]<namespace>/<name>/<system>[//<subdir>]`, and nothing go-getter
// would claim first.
func IsRegistry(source string) bool {
	_, ok := RegistryParts(source)
	return ok
}

// RegistryParts returns a registry address's host, namespace, name and
// system, the host filled in if the address leaves it out. ok is false if
// source isn't a registry address.
func RegistryParts(source string) (parts []string, ok bool) {
	if IsLocal(source) || strings.Contains(source, "::") || strings.Contains(source, "://") {
		return nil, false
	}
	base, _ := SplitSubdir(source)
	if strings.Contains(base, "?") {
		return nil, false
	}
	parts = strings.Split(base, "/")
	switch len(parts) {
	case 3:
		// A namespace has no dots; `github.com/<owner>/<repo>` is a shorthand.
		if strings.Contains(parts[0], ".") {
			return nil, false
		}
		parts = append([]string{DefaultRegistryHost}, parts...)
	case 4:
		if !strings.Contains(parts[0], ".") {
			return nil, false
		}
		// go-getter's shorthands for these hosts win over the registry.
		switch strings.ToLower(parts[0]) {
		case "github.com", "bitbucket.org":
			return nil, false
		}
	default:
		return nil, false
	}
	for _, p := range parts {
		if p == "" {
			return nil, false
		}
	}
	return parts, true
}

// NormalizeSource is the form in which two sources are compared: registry
// addresses gain the default host and are lower-cased, as tofu treats them,
// with the subdir kept as written. Other sources are compared as written.
func NormalizeSource(source string) string {
	parts, ok := RegistryParts(source)
	if !ok {
		return source
	}
	normalized := strings.ToLower(strings.Join(parts, "/"))
	if _, subdir := SplitSubdir(source); subdir != "" {
		normalized += "//" + subdir
	}
	return normalized
}

// Match reports whether a module whose address is provider supplies a call
// to source, and in which subdirectory of provider's directory: source must
// be provider, or provider with a deeper `//<subdir>`.
func Match(provider, source string) (subdir string, ok bool) {
	if NormalizeSource(provider) == NormalizeSource(source) {
		return "", true
	}
	pBase, pSub := SplitSubdir(provider)
	sBase, sSub := SplitSubdir(source)
	if NormalizeSource(pBase) != NormalizeSource(sBase) {
		return "", false
	}
	switch {
	case pSub == sSub:
		return "", true
	case pSub == "":
		return sSub, true
	case strings.HasPrefix(sSub, pSub+"/"):
		return sSub[len(pSub)+1:], true
	}
	return "", false
}

var (
	moduleSchema    = &hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: "module", LabelNames: []string{"name"}}}}
	sourceSchema    = &hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "source"}}}
	terraformSchema = &hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: "terraform"}}}
	requiredSchema  = &hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: "required_providers"}}}
)

// configFiles lists the .tf and .tf.json files in dir: primary files, then
// `_override` files, which tofu merges over them.
func configFiles(dir string) (primary, overrides []string, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !(strings.HasSuffix(name, ".tf") || strings.HasSuffix(name, ".tf.json")) {
			continue
		}
		stem := strings.TrimSuffix(strings.TrimSuffix(name, ".json"), ".tf")
		if stem == "override" || strings.HasSuffix(stem, "_override") {
			overrides = append(overrides, filepath.Join(dir, name))
		} else {
			primary = append(primary, filepath.Join(dir, name))
		}
	}
	return primary, overrides, nil
}

// HasConfig reports whether dir holds any .tf or .tf.json file.
func HasConfig(dir string) bool {
	primary, overrides, err := configFiles(dir)
	return err == nil && len(primary)+len(overrides) > 0
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

// ReadCalls parses the module blocks of the .tf and .tf.json files in dir. An
// `_override` file's `source` replaces the one in the primary file, as tofu
// merges it.
func ReadCalls(dir string) ([]Call, error) {
	primary, overrides, err := configFiles(dir)
	if err != nil {
		return nil, err
	}
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

// RequiredProviders returns the `source` of every required_providers entry in
// dir's .tf and .tf.json files, normalised as `<host>/<namespace>/<name>` in
// lower case, sorted. An entry without a source is `hashicorp/<name>`, as tofu
// implies it.
func RequiredProviders(dir string) ([]string, error) {
	primary, overrides, err := configFiles(dir)
	if err != nil {
		return nil, err
	}
	parser := hclparse.NewParser()
	seen := map[string]bool{}
	for _, file := range append(primary, overrides...) {
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
				for name, attr := range attrs {
					source := "hashicorp/" + name
					if pairs, diags := hcl.ExprMap(attr.Expr); !diags.HasErrors() {
						for _, pair := range pairs {
							key, ok := LiteralString(pair.Key)
							if !ok {
								key = hcl.ExprAsKeyword(pair.Key)
							}
							if key == "source" {
								if value, ok := LiteralString(pair.Value); ok {
									source = value
								}
							}
						}
					}
					seen[NormalizeProvider(source)] = true
				}
			}
		}
	}
	sources := make([]string, 0, len(seen))
	for s := range seen {
		sources = append(sources, s)
	}
	sort.Strings(sources)
	return sources, nil
}

// NormalizeProvider gives a provider source address its host, lower-cased:
// `hashicorp/aws` is `registry.opentofu.org/hashicorp/aws`.
func NormalizeProvider(source string) string {
	source = strings.ToLower(source)
	if strings.Count(source, "/") == 1 {
		source = DefaultRegistryHost + "/" + source
	}
	return source
}

// LiteralString evaluates expr with no variables in scope, as a string.
func LiteralString(expr hcl.Expression) (string, bool) {
	v, diags := expr.Value(nil)
	if diags.HasErrors() || v.IsNull() || !v.IsKnown() || v.Type().FriendlyName() != "string" {
		return "", false
	}
	return v.AsString(), true
}

// ModuleDirs lists the directories that a module's library serves, relative
// to repo: the module's own (moduleDir, relative to repo, "" for its root),
// every directory below its `modules/` (the standard place for submodules)
// that holds configuration, and every directory those reach with local
// `source` paths that stay inside repo. Examples and tests are left out
// unless something calls them. The result is sorted.
func ModuleDirs(repo, moduleDir string) ([]string, error) {
	moduleDir = strings.Trim(moduleDir, "/")
	if !HasConfig(filepath.Join(repo, filepath.FromSlash(moduleDir))) {
		return nil, fmt.Errorf("%q holds no .tf or .tf.json files", "/"+moduleDir)
	}
	pending := []string{moduleDir}
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
		if dir == "." {
			dir = ""
		}
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
			if child := path.Join(dir, c.Source); child != ".." && !strings.HasPrefix(child, "../") {
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
