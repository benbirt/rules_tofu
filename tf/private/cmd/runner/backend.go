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
// module_dependency_<version>_test exercises it across tested_versions (the
// floor and the newest pinned release).
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

// ephemeralExplanation explains why state in Bazel's output tree is unsafe;
// optInAdvice explains allow_ephemeral_state.
const (
	ephemeralExplanation = `State under bazel-out is machine-local, forks across build configurations and
checkouts, and is deleted by "bazel clean". Losing or forking that state file
orphans the resources this command creates.`

	optInAdvice = `For demos, throwaway sandboxes or state-bucket bootstrap, opt in on the
tf_deploy target:

    allow_ephemeral_state = True

After bootstrapping, add the backend or cloud {} block and migrate state in the
run directory above: run "tofu init -migrate-state" for a backend block, or run
"tofu init" and answer its prompt for a cloud {} block, then remove
allow_ephemeral_state. The .plan target is never gated.`
)

// ephemeralStateError formats the refusal for apply or destroy when the deploy
// declares no backend or cloud block.
func ephemeralStateError(command, cwd string) error {
	return fmt.Errorf(`rules_tofu: refusing to run "tofu %s": this deploy declares no state backend, so tofu writes %s in the run directory:

    %s

%s

Declare a remote backend or cloud {} block in this deploy's srcs:

    terraform {
      backend "gcs" {
        bucket = "my-tofu-state"
        prefix = "prod"
      }
    }

backend "local" counts only when path and workspace_dir are both absolute paths
outside Bazel's output tree, and its state remains single-machine.

%s`,
		command, defaultStateFile, cwd, ephemeralExplanation, optInAdvice)
}

// localBackendError formats the refusal for apply or destroy when a local
// backend's paths are relative or resolve inside Bazel's output tree.
func localBackendError(command, reason, cwd string) error {
	return fmt.Errorf(`rules_tofu: refusing to run "tofu %s": this deploy's backend "local" does not keep state outside Bazel's output tree: %s.

tofu resolves relative and default paths from the run directory:

    %s

%s

Set both path and workspace_dir to absolute paths outside Bazel's output tree,
which still keeps state on one machine, or declare a remote backend or cloud {}
block:

    terraform {
      backend "local" {
        path          = "/var/lib/tofu/prod/terraform.tfstate"
        workspace_dir = "/var/lib/tofu/prod/workspaces"
      }
    }

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
