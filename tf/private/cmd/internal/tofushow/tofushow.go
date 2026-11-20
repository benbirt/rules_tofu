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
// rejects output missing `root_module`, a call's `source` or a provider's
// `full_name`, or carrying conflicting version constraints for one provider.
package tofushow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
	// Tofu is the absolute path to the `tofu` binary. ReadFiles changes the
	// subprocess working directory, so a relative path is not safe.
	Tofu string
}

// LoadError reports that `tofu` exited non-zero while loading a module.
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
	cmd := exec.Command(r.Tofu, "show", "-json", "-no-color", "-module="+arg)
	cmd.Dir = root
	cmd.Env = environ()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, &LoadError{Dir: dir, Diagnostics: strings.TrimSpace(stderr.String())}
		}
		return nil, fmt.Errorf("run tofu show -module: %w", err)
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
// temporary directory. Callers must pass configuration files only: when dir is
// "", the staged directory is `tofu`'s working directory, where auto-loaded
// .tfvars files would take effect. files need not share a source directory, so
// long as their base names are distinct.
//
// Staging into a scratch directory serves three purposes:
//   - OpenTofu reads whole directories; staging lets strict-deps checks read
//     one target's files without other targets' files in the same directory.
//   - `tofu show` loads terraform.tfvars[.json] and *.auto.tfvars[.json] from
//     its working directory (not the module directory), which would alter an
//     evaluated `source`. The scratch root has none, and environ strips
//     TF_VAR_* and TF_CLI_ARGS*, so variables in `source` always take their
//     defaults.
//   - Staging under dir makes OpenTofu's diagnostics report paths as
//     <dir>/<file> (or <file> when dir is "").
//
// OpenTofu loads only the staged files: it rejects an override file when the
// block it overrides is not among files, ignores dot-prefixed file names, and
// lets .tofu and .tofu.json shadow .tf and .tf.json of the same stem.
//
// If `tofu` exits non-zero, ReadFiles returns a *LoadError.
func (r Reader) ReadFiles(dir string, files []string) (*Module, error) {
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
		target, err := filepath.Abs(f)
		if err != nil {
			return nil, err
		}
		if err := os.Symlink(target, filepath.Join(staged, filepath.Base(f))); err != nil {
			return nil, err
		}
	}
	return r.show(scratch, dir)
}

// environ returns os.Environ() without TF_VAR_* and TF_CLI_ARGS* variables, so
// input variables and injected CLI flags cannot affect what `tofu show`
// reports.
func environ() []string {
	var env []string
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "TF_VAR_") || strings.HasPrefix(e, "TF_CLI_ARGS") {
			continue
		}
		env = append(env, e)
	}
	return env
}

// Parse decodes the JSON output of `tofu show -json -module=<dir>`. Multiple
// provider_config entries can share a full_name (for example, aliased provider
// blocks); OpenTofu computes them from one requirement per provider, so Parse
// merges them by full_name and rejects distinct non-empty version_constraint
// values for the same full_name.
func Parse(output []byte) (*Module, error) {
	var config struct {
		ProviderConfig map[string]struct {
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
	m := &Module{}
	for name, c := range config.RootModule.ModuleCalls {
		if c.Source == "" {
			return nil, fmt.Errorf("unexpected output: module call %q has no source", name)
		}
		m.Calls = append(m.Calls, Call{Name: name, Source: c.Source, Version: c.VersionConstraint})
	}
	sort.Slice(m.Calls, func(i, j int) bool { return m.Calls[i].Name < m.Calls[j].Name })

	versions := map[string]string{}
	for key, p := range config.ProviderConfig {
		if p.FullName == "" {
			return nil, fmt.Errorf("unexpected output: provider_config %q has no full_name", key)
		}
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
