// Package tfconfig handles the OpenTofu file, address and HCL work that
// rules_tofu does without running `tofu`: configuration and test file sets,
// test-file `run` block calls, module directory traversal, module source and
// provider addresses, and the shared stand-alone load error hint.
package tfconfig

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/benbirt/rules_tofu/tf/private/cmd/internal/tofushow"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	regaddr "github.com/opentofu/registry-address/v2"
)

// Call is a module call from a test file's `run` block. Name is the run label
// and Range is the source attribute's location.
type Call struct {
	Name   string
	Source string
	Range  hcl.Range
}

var sourceSchema = &hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "source"}}}

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

// Effective drops the files in files that `tofu` ignores: each `.tf` or
// `.tf.json` file shadowed by a `.tofu` or `.tofu.json` file of the same path,
// and each whose base name starts with a dot (see ignored). The order of the
// remaining files is kept.
func Effective(files []string) []string {
	present := map[string]bool{}
	for _, f := range files {
		present[f] = true
	}
	var kept []string
	for _, f := range files {
		if ignored(filepath.Base(f)) {
			continue
		}
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
// drops. Unlike TestFiles, it returns an error if dir does not exist.
func ConfigFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && IsConfigFile(e.Name()) {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	return Effective(files), nil
}

// ignored reports whether tofu passes over a file of this name when it reads
// a directory: one that starts with a dot. OpenTofu also passes over editors'
// name~ and #name# backup files, which no configuration or test file name can
// match.
func ignored(name string) bool {
	return strings.HasPrefix(name, ".")
}

// HasConfig reports whether dir is readable and holds any configuration file.
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

// IsBuiltInProvider reports whether source is a provider built into OpenTofu
// (`terraform.io/builtin/*` or `opentofu.org/builtin/*`), which has no plugin.
func IsBuiltInProvider(source string) bool {
	p, err := regaddr.ParseProviderSource(source)
	return err == nil && p.IsBuiltIn()
}

// LiteralString evaluates expr with no variables in scope as a string,
// returning ("", false) if expr is null, unknown, not a string or references
// variables. The context is empty rather than nil: with a nil one the JSON
// syntax returns a template such as "${path.module}/x" as literal text, where
// the native syntax reports the unknown variable, and both must be rejected
// alike.
func LiteralString(expr hcl.Expression) (string, bool) {
	v, diags := expr.Value(&hcl.EvalContext{})
	if diags.HasErrors() || v.IsNull() || !v.IsKnown() || v.Type().FriendlyName() != "string" {
		return "", false
	}
	return v.AsString(), true
}

// ModuleDirs lists the directories that a module's library serves, relative to
// repo, sorted. An empty string stands for repo itself:
//   - moduleDir itself, relative to repo, if it holds configuration;
//   - every directory below <moduleDir>/modules that holds configuration;
//   - each directory in called, relative to repo, that holds configuration;
//   - every directory inside repo that those reach through local source
//     paths.
//
// modules/ is the standard place for submodules; examples and tests are left
// out unless something calls them. The called list supplies submodules outside
// modules/: those the module calls by its own address, and those its
// tf_modules.module tag names.
//
// A readable moduleDir that holds no configuration is a bundle of submodules
// and serves the rest all the same. Whether such a module is acceptable is the
// caller's to say: modscan refuses a fetched one. A missing or unreadable
// moduleDir returns an error.
//
// sources returns the `source` of every module call in dir, where dir is
// relative to repo and "" is repo itself. ModuleDirs calls sources once per
// visited directory, including targets of local paths whose existence it has
// not checked, and returns any error unchanged.
func ModuleDirs(repo, moduleDir string, called []string, sources func(dir string) ([]string, error)) ([]string, error) {
	moduleDir = strings.Trim(moduleDir, "/")
	// The directory itself must be there and readable: a module that isn't
	// is an error, not a bundle.
	own, err := ConfigFiles(filepath.Join(repo, filepath.FromSlash(moduleDir)))
	if err != nil {
		return nil, err
	}
	var pending []string
	if len(own) > 0 {
		pending = append(pending, moduleDir)
	}
	for _, dir := range called {
		dir = strings.Trim(dir, "/")
		// A called directory without configuration fails the caller's build,
		// which says so; it isn't ours to report.
		if HasConfig(filepath.Join(repo, filepath.FromSlash(dir))) {
			pending = append(pending, dir)
		}
	}
	submodules := filepath.Join(repo, filepath.FromSlash(path.Join(moduleDir, "modules")))
	err = filepath.WalkDir(submodules, func(p string, d fs.DirEntry, err error) error {
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
		found, err := sources(dir)
		if err != nil {
			return nil, err
		}
		for _, source := range found {
			if !IsLocal(source) {
				continue
			}
			// A path out of the repository fails in tofu; it isn't ours to report.
			if child := ResolveLocal(dir, source); child != ".." && !strings.HasPrefix(child, "../") {
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

// IsOverride reports whether file base name name is an OpenTofu override file:
// not dot-prefixed, with a configuration extension, and `override` or
// `*_override` as its stem.
func IsOverride(name string) bool {
	if ignored(name) || configExt(name) == "" {
		return false
	}
	stem := strings.TrimSuffix(name, configExt(name))
	return stem == "override" || strings.HasSuffix(stem, "_override")
}

// StandAloneHint returns a hint to append when OpenTofu fails to load one
// target's configuration files in a directory on their own. It returns ""
// unless err wraps a *tofushow.LoadError and files includes an override file.
// Because the load failure may have another cause, callers append this hint
// rather than replacing OpenTofu's diagnostics.
func StandAloneHint(label string, err error, files []string) string {
	var load *tofushow.LoadError
	if !errors.As(err, &load) || !slices.ContainsFunc(files, func(f string) bool { return IsOverride(filepath.Base(f)) }) {
		return ""
	}
	return fmt.Sprintf("\n\nOpenTofu read the files of %s in this directory without any other target's files. "+
		"An override file must be in the same target as the block it overrides.", label)
}
