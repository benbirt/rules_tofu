// Package tofushow reads a module's calls and required providers by running
// `tofu show -json -no-color -module=<dir>` (added in OpenTofu 1.11). OpenTofu
// loads the directory without `tofu init`, provider plugins or network access:
// it merges override files, lets .tofu and .tofu.json files shadow .tf and
// .tf.json files, reads JSON configuration, and statically evaluates `module`
// `source` expressions that refer to local values or to variables with
// defaults.
//
// OpenTofu documents the `-module` output as a subset of its JSON
// configuration representation, without a `format_version` field. Parse
// rejects output missing `root_module`, a call's `source`, or a provider's
// `name` or `full_name`, as well as one local name mapped to two providers or
// conflicting version constraints for one provider.
package tofushow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Module holds the module calls and provider requirements OpenTofu reports for
// one module directory.
type Module struct {
	// Calls holds the module's `module` blocks, sorted by Name.
	Calls []Call
	// Providers holds every provider the module requires, sorted by Source:
	// entries in required_providers and providers implied by a resource, data
	// source or provider block. Built-in providers are included.
	Providers []Provider
	// LocalNames maps each provider local name reported for the module to the
	// canonical address OpenTofu resolves it to with the staged files alone.
	LocalNames map[string]string
}

// Call is a `module` block reported by OpenTofu.
type Call struct {
	Name string
	// Source is the effective source string after OpenTofu merges override
	// files and evaluates any expression, without normalising the address.
	Source string
	// Version is the call's version constraint, or "".
	Version string
}

// Provider is a provider required by a module.
type Provider struct {
	// Source is the canonical <host>/<namespace>/<name> address.
	Source string
	// Version is the module's version constraint for Source, or "".
	Version string
}

// Reader runs `tofu` to inspect module directories.
type Reader struct {
	// Tofu is the path to the `tofu` binary, required.
	Tofu string
}

// LoadError reports that `tofu` exited non-zero with diagnostics while loading
// a module.
type LoadError struct {
	// Dir is the directory argument passed to ReadFiles.
	Dir string
	// Diagnostics is trimmed standard error from `tofu`, naming each file as
	// Dir joined with its base name.
	Diagnostics string
}

func (e *LoadError) Error() string {
	dir := e.Dir
	if dir == "" {
		dir = "."
	}
	return fmt.Sprintf("tofu cannot load the module in %q:\n\n%s", dir, e.Diagnostics)
}

// show runs `tofu show -json -no-color -module=<dir>` with root as its working
// directory. dir is slash-separated and relative to root, or "" for root
// itself. OpenTofu formats file paths in diagnostics relative to its working
// directory.
func (r Reader) show(root, dir string) (*Module, error) {
	arg := filepath.FromSlash(dir)
	if arg == "" {
		arg = "."
	}
	if r.Tofu == "" {
		return nil, errors.New("no tofu binary to run")
	}
	// cmd.Dir changes the working directory that a relative path would be
	// resolved against.
	tofu, err := filepath.Abs(r.Tofu)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(tofu, "show", "-json", "-no-color", "-module="+arg)
	cmd.Dir = root
	cmd.Env = environ(root)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		diagnostics := strings.TrimSpace(stderr.String())
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.Exited() && diagnostics != "" {
			return nil, &LoadError{Dir: dir, Diagnostics: diagnostics}
		}
		return nil, fmt.Errorf("run tofu show -json -module=%s: %w", arg, err)
	}
	m, err := Parse(stdout.Bytes())
	if err != nil {
		return nil, fmt.Errorf("tofu show -json -module=%s: %w", arg, err)
	}
	return m, nil
}

// ReadFiles symlinks configuration files into a temporary directory under
// slash-separated path dir (or the scratch root when dir is ""), runs `tofu
// show` with the scratch root as its working directory, and removes the
// temporary directory. dir must be "" or a clean relative path that stays
// inside the scratch root. files need not share a source directory, so long as
// their base names are distinct. ReadFiles rejects .tfvars and .tfvars.json
// files: when dir is "", the staged directory is `tofu`'s working directory,
// where auto-loaded variable files would take effect.
//
// Staging into a scratch directory serves three purposes:
//   - OpenTofu reads whole directories; staging lets strict-deps checks read
//     one target's files without other targets' files in the same directory.
//   - `tofu show` loads terraform.tfvars[.json] and *.auto.tfvars[.json] from
//     its working directory (not the module directory), which would alter an
//     evaluated `source`. The scratch root has none, and environ strips TF_*
//     variables, HOME and XDG_CONFIG_HOME (setting HOME to the scratch root),
//     so variables in `source` always take their defaults and user CLI config
//     or TF_LOG cannot interfere.
//   - Staging under dir makes OpenTofu's diagnostics report paths as
//     <dir>/<file> (or <file> when dir is "").
//
// OpenTofu loads only the staged files: it rejects an override file when the
// block it overrides is not among files, ignores dot-prefixed file names, and
// lets .tofu and .tofu.json shadow .tf and .tf.json of the same stem. A
// non-built-in provider local name with no required_providers entry among
// files is reported as registry.opentofu.org/hashicorp/<name>.
//
// If `tofu` exits non-zero with diagnostics, ReadFiles returns a *LoadError; a
// signal or empty stderr returns an ordinary error.
func (r Reader) ReadFiles(dir string, files []string) (*Module, error) {
	if dir != "" && (path.Clean(dir) != dir || path.IsAbs(dir) || dir == ".." || strings.HasPrefix(dir, "../")) {
		return nil, fmt.Errorf("directory %q is not a clean relative path", dir)
	}
	scratch, err := os.MkdirTemp("", "rules_tofu-show-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(scratch)
	staged := filepath.Join(scratch, filepath.FromSlash(dir))
	if err := os.MkdirAll(staged, 0o755); err != nil {
		return nil, err
	}
	for _, f := range files {
		base := filepath.Base(f)
		if strings.HasSuffix(base, ".tfvars") || strings.HasSuffix(base, ".tfvars.json") {
			return nil, fmt.Errorf("%s is a variables file, not configuration", f)
		}
		target, err := filepath.Abs(f)
		if err != nil {
			return nil, err
		}
		if err := os.Symlink(target, filepath.Join(staged, base)); err != nil {
			return nil, err
		}
	}
	return r.show(scratch, dir)
}

// environ returns the environment for `tofu show` run in scratch: os.Environ()
// without any TF_* variable, HOME or XDG_CONFIG_HOME, plus TF_IN_AUTOMATION=1
// and HOME=<scratch>. Input variables (TF_VAR_*), injected flags
// (TF_CLI_ARGS*), logging (TF_LOG*) and CLI configuration files in the user's
// home or XDG config directory or named by TF_CLI_CONFIG_FILE then cannot
// affect what `tofu` reports or prints.
func environ(scratch string) []string {
	var env []string
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "TF_") || strings.HasPrefix(e, "HOME=") || strings.HasPrefix(e, "XDG_CONFIG_HOME=") {
			continue
		}
		env = append(env, e)
	}
	return append(env, "TF_IN_AUTOMATION=1", "HOME="+scratch)
}

// Parse decodes the JSON output of `tofu show -json -module=<dir>`. Multiple
// provider_config entries can share a full_name (for example, aliased provider
// blocks or two local names for one provider); OpenTofu gives each of them the
// combined version_constraint for that provider, so Parse merges them by
// full_name and rejects distinct non-empty version_constraint values as
// unexpected output. It also rejects missing name or full_name fields and a
// local name that two entries give to different providers.
func Parse(output []byte) (*Module, error) {
	var config struct {
		ProviderConfig map[string]struct {
			Name              string `json:"name"`
			FullName          string `json:"full_name"`
			VersionConstraint string `json:"version_constraint"`
		} `json:"provider_config"`
		RootModule *struct {
			ModuleCalls map[string]struct {
				Source            string `json:"source"`
				VersionConstraint string `json:"version_constraint"`
			} `json:"module_calls"`
		} `json:"root_module"`
	}
	if err := json.Unmarshal(output, &config); err != nil {
		return nil, fmt.Errorf("unexpected output: %w", err)
	}
	if config.RootModule == nil {
		return nil, errors.New("unexpected output: no root_module")
	}
	m := &Module{LocalNames: map[string]string{}}
	for name, c := range config.RootModule.ModuleCalls {
		if c.Source == "" {
			return nil, fmt.Errorf("unexpected output: module call %q has no source", name)
		}
		m.Calls = append(m.Calls, Call{Name: name, Source: c.Source, Version: c.VersionConstraint})
	}
	sort.Slice(m.Calls, func(i, j int) bool { return m.Calls[i].Name < m.Calls[j].Name })

	versions := map[string]string{}
	for key, p := range config.ProviderConfig {
		if p.Name == "" || p.FullName == "" {
			return nil, fmt.Errorf("unexpected output: provider_config %q has no name or no full_name", key)
		}
		if other, ok := m.LocalNames[p.Name]; ok && other != p.FullName {
			return nil, fmt.Errorf("unexpected output: provider local name %q is both %s and %s", p.Name, min(other, p.FullName), max(other, p.FullName))
		}
		m.LocalNames[p.Name] = p.FullName
		switch seen := versions[p.FullName]; {
		case seen == "":
			versions[p.FullName] = p.VersionConstraint
		case p.VersionConstraint != "" && p.VersionConstraint != seen:
			return nil, fmt.Errorf("unexpected output: provider %s has version constraints %q and %q", p.FullName, min(seen, p.VersionConstraint), max(seen, p.VersionConstraint))
		}
	}
	for source, version := range versions {
		m.Providers = append(m.Providers, Provider{Source: source, Version: version})
	}
	sort.Slice(m.Providers, func(i, j int) bool { return m.Providers[i].Source < m.Providers[j].Source })
	return m, nil
}
