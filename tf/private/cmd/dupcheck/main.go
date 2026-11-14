// Command dupcheck checks at build time that variable names do not collide
// across a tf_deploy's `vars` dict and its `var_files` entries.
//
// tf_deploy invokes it as a Bazel build action when `var_files` is non-empty:
//   - --vars-key=<name>, repeatable: one entry per key in the deploy's `vars`
//     dict, known statically.
//   - --var-file=<path>, repeatable: a `.tfvars.json` or `.tfvars` file whose
//     top-level variable assignments are checked.
//   - --var-file-label=<label>, repeatable: display name for the --var-file
//     at the same position, typically its workspace-relative path. Must be
//     given once per --var-file. The two travel separately because a colon is
//     legal in a Bazel target name, and so in both, which makes a `label:path`
//     string ambiguous (issue #62).
//   - --stamp=<path>, required: empty file written on success.
//
// On success it writes --stamp and prints nothing. On a duplicate key, an
// unreadable or malformed file or invalid flags, it prints `rules_tofu: <err>`
// to stderr and exits 1 without writing --stamp.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/benbirt/rules_tofu/tf/private/cmd/internal/flags"
	"github.com/hashicorp/hcl/v2/hclparse"
)

var (
	stamp         = flag.String("stamp", "", "path to a stamp file to touch on success")
	varsKeys      = flags.StringList("vars-key", "key from the deploy's `vars` dict (repeatable)")
	varFiles      = flags.StringList("var-file", "`path` of a .tfvars.json or .tfvars file (repeatable)")
	varFileLabels = flags.StringList("var-file-label", "human-readable `label` for the --var-file at the same position (repeatable)")
)

func main() {
	flag.Parse()

	if err := run(*varsKeys, *varFiles, *varFileLabels, *stamp); err != nil {
		fmt.Fprintln(os.Stderr, "rules_tofu: "+err.Error())
		os.Exit(1)
	}
}

func run(varsKeys, varFiles, varFileLabels []string, stamp string) error {
	if stamp == "" {
		return errors.New("--stamp is required")
	}
	if len(varFileLabels) != len(varFiles) {
		return fmt.Errorf(
			"--var-file-label must be given once per --var-file: got %d label(s) for %d file(s)",
			len(varFileLabels), len(varFiles),
		)
	}

	seen := make(map[string]string, len(varsKeys))
	for _, k := range varsKeys {
		if other, dup := seen[k]; dup {
			return fmt.Errorf(
				"variable %q is declared twice in %s — this should be unreachable for a Starlark dict",
				k, other,
			)
		}
		seen[k] = "vars"
	}

	for i, path := range varFiles {
		label := varFileLabels[i]
		keys, err := readVarFileKeys(path)
		if err != nil {
			return fmt.Errorf("read var_file %s: %w", label, err)
		}
		for _, k := range keys {
			if other, dup := seen[k]; dup {
				return fmt.Errorf(
					"variable %q is declared in both %s and %s — "+
						"keys in var_files must not overlap with vars or each other",
					k, other, label,
				)
			}
			seen[k] = label
		}
	}

	return os.WriteFile(stamp, nil, 0o644)
}

// readVarFileKeys returns the top-level variable names assigned in path.
// Files ending in .json are parsed as JSON objects; all others are parsed as
// HCL attributes.
func readVarFileKeys(path string) ([]string, error) {
	if strings.HasSuffix(path, ".json") {
		return readJSONKeys(path)
	}
	return readHCLKeys(path)
}

func readJSONKeys(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	return keys, nil
}

func readHCLKeys(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	parser := hclparse.NewParser()
	file, diags := parser.ParseHCL(data, path)
	if diags.HasErrors() {
		return nil, fmt.Errorf("parse: %w", diags)
	}
	attrs, diags := file.Body.JustAttributes()
	if diags.HasErrors() {
		return nil, fmt.Errorf("parse: %w", diags)
	}
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	return keys, nil
}
