package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// defaultStateFile is OpenTofu's default local state filename in the working
// directory, used when no backend or cloud block is declared and when a local
// backend's path is unset.
const defaultStateFile = "terraform.tfstate"

// backendRecordPath returns the file where tofu init records the backend it
// configured for cwd. TF_DATA_DIR is pinned to cwd/.terraform.
func backendRecordPath(cwd string) string {
	return filepath.Join(cwd, ".terraform", "terraform.tfstate")
}

// backendRecord is the backend metadata written by tofu init. Type is the
// label of a backend block or "cloud" for a cloud block; Config holds the
// block's settings after OpenTofu merged override files and evaluated
// expressions.
type backendRecord struct {
	Type   string          `json:"type"`
	Config json.RawMessage `json:"config"`
}

// recordedBackend reads the backend recorded by tofu init in cwd. It returns
// (nil, nil) if the file is absent or names no backend, which means the root
// module has no backend or cloud block and keeps state in cwd.
//
// Because the previous run's record is removed before tofu init, any file
// present was written by this run. The file format is internal to OpenTofu;
// module_dependency_<version>_test exercises it on every pinned release.
func recordedBackend(cwd string) (*backendRecord, error) {
	path := backendRecordPath(cwd)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var record struct {
		Backend *backendRecord `json:"backend"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if record.Backend == nil || record.Backend.Type == "" {
		return nil, nil
	}
	return record.Backend, nil
}

// checkStateDurable checks the backend that tofu init recorded in cwd and
// refuses command ("apply" or "destroy") unless state is durable:
//   - no recorded backend: ephemeral, because tofu writes state in cwd;
//   - type "local": durable only when localBackendEphemeral returns "";
//   - any other type (remote backends and "cloud"): durable.
//
// An unreadable or malformed record is also refused.
func checkStateDurable(command, cwd, outputRoot string) error {
	backend, err := recordedBackend(cwd)
	if err != nil {
		return fmt.Errorf("rules_tofu: refusing to run \"tofu %s\": can't tell where state is stored: %w", command, err)
	}
	if backend == nil {
		return ephemeralStateError(command, cwd)
	}
	if backend.Type != "local" {
		return nil
	}
	reason, err := localBackendEphemeral(backend.Config, outputRoot)
	if err != nil {
		return fmt.Errorf("rules_tofu: refusing to run \"tofu %s\": can't tell where backend \"local\" stores state: %w", command, err)
	}
	if reason != "" {
		return localBackendError(command, reason, cwd)
	}
	return nil
}

// localBackendEphemeral checks the JSON config recorded for a local backend
// against Bazel's output tree outputRoot. It returns "" when state is durable,
// or a reason string explaining why it is not.
//
// Both path (default workspace state) and workspace_dir (named workspace
// state) must be absolute paths that resolve outside outputRoot after
// following symlinks, such as a checkout's bazel-bin symlink. Null or empty
// attributes use OpenTofu's relative defaults (terraform.tfstate and
// terraform.tfstate.d), which resolve inside the run directory.
func localBackendEphemeral(config json.RawMessage, outputRoot string) (string, error) {
	var settings struct {
		Path         *string `json:"path"`
		WorkspaceDir *string `json:"workspace_dir"`
	}
	if err := json.Unmarshal(config, &settings); err != nil {
		return "", fmt.Errorf("parse the settings tofu init recorded: %w", err)
	}
	root, err := evalSymlinksAllowMissing(outputRoot)
	if err != nil {
		return "", fmt.Errorf("resolve Bazel's output tree (%s): %w", outputRoot, err)
	}
	for _, attr := range []struct {
		name, def string
		value     *string
	}{
		{"path", defaultStateFile, settings.Path},
		{"workspace_dir", "terraform.tfstate.d", settings.WorkspaceDir},
	} {
		if attr.value == nil || *attr.value == "" {
			return fmt.Sprintf("its `%s` is not set, and tofu's default for it (%q) is relative", attr.name, attr.def), nil
		}
		v := *attr.value
		if !filepath.IsAbs(v) {
			return fmt.Sprintf("its `%s` (%q) is relative", attr.name, v), nil
		}
		resolved, err := evalSymlinksAllowMissing(v)
		if err != nil {
			return "", fmt.Errorf("resolve its `%s` (%q): %w", attr.name, v, err)
		}
		if within(root, resolved) {
			return fmt.Sprintf("its `%s` (%q) is inside Bazel's output tree (%s)", attr.name, v, root), nil
		}
	}
	return "", nil
}

// ephemeralExplanation completes a sentence explaining why state in the output
// tree is unsafe; optInAdvice explains allow_ephemeral_state.
const (
	ephemeralExplanation = `is machine-local and disposable. It sits under bazel-bin, so
"bazel clean" deletes it; it embeds the build configuration, so building with
-c opt or a different --platforms silently forks to a different state file; and
a colleague running this target from their own checkout gets their own private
copy. Losing or forking that file orphans every resource this command creates:
the infrastructure carries on existing with nothing tracking it.`

	optInAdvice = `If disposable state is the point — a demo or example, a throwaway sandbox, or
bootstrapping the bucket that will later hold real state — opt in explicitly by
setting

    allow_ephemeral_state = True

on the tf_deploy target. For the bootstrap case, add the remote backend block
once the bucket exists and move the existing state into it by running this in
the run directory above:

    tofu init -migrate-state

The deploy's .plan target is never gated, so planning still works either way.`
)

// ephemeralStateError formats the refusal for apply or destroy when the deploy
// declares no backend or cloud block.
func ephemeralStateError(command, cwd string) error {
	return fmt.Errorf(`rules_tofu: refusing to run "tofu %s": this deploy declares no state backend, so tofu writes %s in the run directory:

    %s

State there %s

Recommended fix — declare a remote backend in a .tf file listed in this
deploy's srcs:

    terraform {
      backend "gcs" {
        bucket = "my-tofu-state"
        prefix = "prod"
      }
    }

(Any remote backend or a cloud {} block will do. A backend "local" block
counts only when its path and workspace_dir are both absolute paths outside
Bazel's output tree, and that state is still local to this machine.)
With one present, the runner defers to it and this check no longer applies.

%s`,
		command, defaultStateFile, cwd, ephemeralExplanation, optInAdvice)
}

// localBackendError formats the refusal for apply or destroy when a local
// backend's paths are relative or resolve inside Bazel's output tree.
func localBackendError(command, reason, cwd string) error {
	return fmt.Errorf(`rules_tofu: refusing to run "tofu %s": this deploy's backend "local" does not keep state outside Bazel's output tree: %s.

tofu resolves that block itself, from this deploy's run directory,

    %s

which is where a relative path, or the default of an attribute left unset,
leads. State there %s

To keep this state, set both path and workspace_dir to absolute paths outside
Bazel's output tree:

    terraform {
      backend "local" {
        path          = "/var/lib/tofu/prod/terraform.tfstate"
        workspace_dir = "/var/lib/tofu/prod/workspaces"
      }
    }

or declare a remote backend (s3, gcs, ...) or a cloud {} block.

%s`,
		command, reason, cwd, ephemeralExplanation, optInAdvice)
}

// evalSymlinksAllowMissing is filepath.EvalSymlinks for a path whose tail may
// not exist yet, as a state file doesn't before the first apply: the longest
// existing ancestor is resolved and the missing components re-joined to it.
func evalSymlinksAllowMissing(p string) (string, error) {
	p = filepath.Clean(p)
	var tail []string
	for {
		resolved, err := filepath.EvalSymlinks(p)
		if err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", err
		}
		tail = append(tail, filepath.Base(p))
		p = parent
	}
}

// within reports whether path is dir or something under it. Both are clean
// and absolute.
func within(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
