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

// defaultStateFile is where tofu keeps the default workspace's state, relative
// to the directory it runs in, when no backend or cloud block says otherwise.
const defaultStateFile = "terraform.tfstate"

// backendRecordPath returns the file in which `tofu init`, run in cwd, records
// the backend it configured. The runner fixes TF_DATA_DIR to cwd/.terraform
// (see tofuEnv).
func backendRecordPath(cwd string) string {
	return filepath.Join(cwd, ".terraform", "terraform.tfstate")
}

// backendRecord is the backend `tofu init` recorded: the type of a
// `backend "<type>"` block, or "cloud" for a `cloud` block, and the block's
// settings as tofu resolved them, with variables and locals evaluated.
type backendRecord struct {
	Type   string          `json:"type"`
	Config json.RawMessage `json:"config"`
}

// recordedBackend reads the backend `tofu init` recorded for cwd, or nil when
// it recorded none: the configuration has no backend or cloud block, so tofu
// keeps state in cwd.
//
// The record is the answer tofu itself gives, after merging override files
// and evaluating expressions, so the runner parses no configuration. Its
// format is internal to tofu, like the module manifest's;
// module_dependency_<version>_test covers it for every pinned release.
// syncRunDir removes the previous run's record before init, so what is read
// here is always this run's.
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

// checkStateDurable returns the refusal for an apply or destroy whose state
// would not outlive `bazel clean`, or nil when it would. It runs after
// `tofu init` in cwd, the root module's directory in the run directory, and
// reads what init recorded:
//   - no record: no backend or cloud block, so state is in cwd;
//   - a `backend "local"`: durable only if it keeps state outside Bazel's
//     output tree outputRoot (see localBackendEphemeral);
//   - anything else, a remote backend or a cloud block: durable.
//
// A record that can't be read is a refusal too.
func checkStateDurable(command, cwd, outputRoot string) error {
	backend, err := recordedBackend(cwd)
	if err != nil {
		return fmt.Errorf("rules_tofu: refusing to run \"tofu %s\": can't tell where this deploy keeps its state: %w", command, err)
	}
	if backend == nil {
		return ephemeralStateError(command, cwd)
	}
	if backend.Type != "local" {
		return nil
	}
	reason, err := localBackendEphemeral(backend.Config, outputRoot)
	if err != nil {
		return fmt.Errorf("rules_tofu: refusing to run \"tofu %s\": can't tell where this deploy's backend \"local\" keeps its state: %w", command, err)
	}
	if reason != "" {
		return localBackendError(command, reason, cwd)
	}
	return nil
}

// localBackendEphemeral decides whether a `backend "local"` keeps its state
// outside Bazel's output tree outputRoot, given the settings tofu recorded
// for it. It returns why not, or "" when it does. The runner never changes
// what the block means: tofu resolves it from the run directory, which is
// under bazel-bin, so anything relative lands there and goes with
// `bazel clean`.
//
// State is outside only when both `path`, which holds the default
// workspace's state, and `workspace_dir`, which holds the other workspaces',
// are absolute paths that do not lead into outputRoot once symlinks are
// followed, such as a checkout's bazel-bin convenience link. For one that is
// null or empty tofu uses its relative default.
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

// ephemeralExplanation says why state under bazel-bin is disposable; each
// refusal supplies the sentence's subject, and optInAdvice explains how to opt
// in.
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
once the bucket exists and move the existing state into it by running, in the
directory above:

    tofu init -migrate-state

The deploy's .plan target is never gated, so planning still works either way.`
)

// ephemeralStateError is the refusal returned when apply/destroy would write
// local state for a deploy with no backend. It is the whole user-facing
// surface of the gate, so it names the state path, the recommended fix and the
// opt-in verbatim.
func ephemeralStateError(command, cwd string) error {
	return fmt.Errorf(`rules_tofu: refusing to run "tofu %s": this deploy declares no state backend, so tofu would write its state to %s in this deploy's run directory,

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
With one present, this check no longer applies.

%s`,
		command, defaultStateFile, cwd, ephemeralExplanation, optInAdvice)
}

// localBackendError is the refusal returned when apply/destroy would run
// against a `backend "local"` that doesn't keep its state outside Bazel's
// output tree. It names the attribute that fails the test and the value tofu
// resolved it to, where tofu runs, the fix and the opt-in.
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
