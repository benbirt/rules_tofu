package tfconfig

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/benbirt/rules_tofu/tf/private/cmd/internal/tofushow"
)

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// A `.tofu` or `.tofu.json` file shadows the `.tf` or `.tf.json` file of the
// same name, and only that one.
func TestConfigFiles_TofuShadowsTf(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"main.tf":            "",
		"main.tofu":          "",
		"main.tf.json":       "{}",
		"data.tf.json":       "{}",
		"data.tofu.json":     "{}",
		"only.tofu":          "",
		"x_override.tf":      "",
		"x_override.tofu":    "",
		"vars.tfvars":        "",
		"notes.tofu.txt":     "",
		"tests.tofutest.hcl": "",
	})
	got, err := ConfigFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := range got {
		got[i] = filepath.Base(got[i])
	}
	want := []string{"data.tofu.json", "main.tf.json", "main.tofu", "only.tofu", "x_override.tofu"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// OpenTofu ignores dotfiles and directories whose names end in configuration
// or test extensions. Checked on OpenTofu 1.12.0: `tofu init` treats a
// directory holding only .hidden.tf as "an empty directory", and
// `tofu validate` ignores the file's contents.
func TestConfigFiles_IgnoresDotfilesAndDirectories(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		".hidden.tf":         ``,
		".tf":                ``,
		".also.tofu.json":    `{}`,
		".hidden.tftest.hcl": ``,
	})
	for _, name := range []string{"dir.tf", "dir.tftest.hcl"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if HasConfig(dir) {
		t.Errorf("HasConfig is true for a directory of dotfiles: %v", mustConfigFiles(t, dir))
	}
	if got, err := TestFiles(dir); err != nil || len(got) != 0 {
		t.Errorf("TestFiles = %v, %v, want none", got, err)
	}
	writeFiles(t, dir, map[string]string{"shown.tf": ``, "shown.tftest.hcl": ``})
	if got := mustConfigFiles(t, dir); len(got) != 1 || filepath.Base(got[0]) != "shown.tf" {
		t.Errorf("ConfigFiles = %v, want only shown.tf", got)
	}
	if got, err := TestFiles(dir); err != nil || len(got) != 1 || filepath.Base(got[0]) != "shown.tftest.hcl" {
		t.Errorf("TestFiles = %v, %v, want only shown.tftest.hcl", got, err)
	}
}

// Effective and EffectiveTests drop dot-prefixed file names while keeping
// files inside dot-prefixed directories.
func TestEffective_DropsDotfiles(t *testing.T) {
	got := Effective([]string{"a/main.tf", "a/._main.tf", "a/.hidden.tofu", ".dir/main.tf", "b/.tf"})
	if want := []string{"a/main.tf", ".dir/main.tf"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Effective = %v, want %v", got, want)
	}
	gotTests := EffectiveTests([]string{"a/x.tftest.hcl", "a/._x.tftest.hcl", "a/.y.tofutest.hcl"})
	if want := []string{"a/x.tftest.hcl"}; !reflect.DeepEqual(gotTests, want) {
		t.Errorf("EffectiveTests = %v, want %v", gotTests, want)
	}
}

func mustConfigFiles(t *testing.T, dir string) []string {
	t.Helper()
	files, err := ConfigFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestHasConfig_TofuOnly(t *testing.T) {
	dir := t.TempDir()
	if HasConfig(dir) {
		t.Error("empty directory has config")
	}
	writeFiles(t, dir, map[string]string{"main.tofu.json": "{}"})
	if !HasConfig(dir) {
		t.Error("a directory holding only main.tofu.json has no config")
	}
}

// writeModules creates an empty main.tf under repo for each directory in calls
// and returns a `sources` callback for ModuleDirs that returns calls[dir] or
// os.Stat's not-exist error when main.tf is absent.
func writeModules(t *testing.T, repo string, calls map[string][]string) func(string) ([]string, error) {
	t.Helper()
	for dir := range calls {
		writeFiles(t, filepath.Join(repo, filepath.FromSlash(dir)), map[string]string{"main.tf": ``})
	}
	return func(dir string) ([]string, error) {
		if _, err := os.Stat(filepath.Join(repo, filepath.FromSlash(dir), "main.tf")); err != nil {
			return nil, err
		}
		return calls[dir], nil
	}
}

// ModuleDirs lists the module's directory, the configured directories under
// its modules/, the called directories that hold configuration and what local
// calls reach, backslash paths included; a call out of the repository is
// ignored, and examples count only when called.
func TestModuleDirs(t *testing.T) {
	repo := t.TempDir()
	sources := writeModules(t, repo, map[string][]string{
		"":                 {`.\lib\a`, "acme/dns/aws"},
		"lib/a":            {`..\..\..\outside`},
		"modules/sub":      {"../../examples/called"},
		"examples/called":  nil,
		"examples/ignored": nil,
		"tools":            {"./deep"},
		"tools/deep":       nil,
	})
	writeFiles(t, filepath.Join(repo, "modules"), map[string]string{"README.md": ``})
	writeFiles(t, filepath.Join(repo, "docs"), map[string]string{"README.md": ``})
	got, err := ModuleDirs(repo, "", nil, sources)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"", "examples/called", "lib/a", "modules/sub"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	// Called directories are followed too; one without configuration is
	// skipped, and the module's own among them changes nothing.
	got, err = ModuleDirs(repo, "", []string{"", "tools/", "docs", "examples/called"}, sources)
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"", "examples/called", "lib/a", "modules/sub", "tools", "tools/deep"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("with called dirs: got %v, want %v", got, want)
	}
}

// A module whose own directory holds no configuration, a bundle, still
// serves its `modules/` and the directories calls reach (issue #123); with
// neither it serves nothing, and that is no error.
func TestModuleDirs_NoRootConfiguration(t *testing.T) {
	repo := t.TempDir()
	sources := writeModules(t, repo, map[string][]string{
		"bundle/modules/dns": {"../../shared"},
		"bundle/shared":      nil,
		"bundle/vpc":         {"../peering"},
		"bundle/peering":     nil,
		"bundle/examples":    nil,
	})
	writeFiles(t, filepath.Join(repo, "bundle"), map[string]string{"README.md": ``})
	writeFiles(t, filepath.Join(repo, "bare"), map[string]string{"BUILD.bazel": ``})
	for name, tc := range map[string]struct {
		moduleDir string
		called    []string
		want      []string
	}{
		"modules/ alone":         {"bundle", nil, []string{"bundle/modules/dns", "bundle/shared"}},
		"and a called directory": {"bundle", []string{"bundle/vpc"}, []string{"bundle/modules/dns", "bundle/peering", "bundle/shared", "bundle/vpc"}},
		"its own root, called":   {"bundle", []string{"bundle"}, []string{"bundle/modules/dns", "bundle/shared"}},
		"nothing at all":         {"bare", nil, []string{}},

		"called alone":            {"bare", []string{"bundle/peering"}, []string{"bundle/peering"}},
		"the repository's root":   {"", []string{"bundle/vpc"}, []string{"bundle/peering", "bundle/vpc"}},
		"called, no config there": {"bare", []string{"bundle/examples/none"}, []string{}},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := ModuleDirs(repo, tc.moduleDir, tc.called, sources)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.want) || (len(got) > 0 && !reflect.DeepEqual(got, tc.want)) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}

	// A module directory that isn't there is not a bundle: that is an error.
	if got, err := ModuleDirs(repo, "nowhere", []string{"bundle/vpc"}, sources); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ModuleDirs of a missing directory = %v, %v, want a not-exist error", got, err)
	}
}

func TestResolveLocal(t *testing.T) {
	for _, tc := range []struct{ dir, source, want string }{
		{"app", "./x", "app/x"},
		{"app", "../shared", "shared"},
		{"app", "./", "app"},
		{"", "./", ""},
		{"a", "../", ""},
		{"a", `..\`, ""},
		{"", "../out", "../out"},
		{"app", `..\sibling`, "sibling"},
		{"", `..\outside`, "../outside"},
		{"app", `.\x\y`, "app/x/y"},
	} {
		if got := ResolveLocal(tc.dir, tc.source); got != tc.want {
			t.Errorf("ResolveLocal(%q, %q) = %q, want %q", tc.dir, tc.source, got, tc.want)
		}
	}
}

func TestIsOverride(t *testing.T) {
	for name, want := range map[string]bool{
		"override.tf":           true,
		"main_override.tf":      true,
		"main_override.tofu":    true,
		"override.tf.json":      true,
		"main_override.tf.json": true,
		"main.tf":               false,
		"overrides.tf":          false,
		// OpenTofu ignores dot-prefixed files.
		"._override.tf":     false,
		".main_override.tf": false,
		"main_override":     false,
		"main_override.txt": false,
	} {
		if got := IsOverride(name); got != want {
			t.Errorf("IsOverride(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestStandAloneHint(t *testing.T) {
	load := fmt.Errorf("//app:app: %w", &tofushow.LoadError{Dir: "app", Diagnostics: "Error: Missing module call to override"})
	for _, tc := range []struct {
		name  string
		err   error
		files []string
		want  bool
	}{
		{"override file that tofu cannot load", load, []string{"/ws/app/main.tf", "/ws/app/x_override.tf"}, true},
		{"no override file", load, []string{"/ws/app/main.tf"}, false},
		{"dot-prefixed override file", load, []string{"/ws/app/._override.tf"}, false},
		{"another kind of error", errors.New("signal: killed"), []string{"/ws/app/override.tf"}, false},
	} {
		got := StandAloneHint("//app:app", tc.err, tc.files)
		if tc.want != strings.Contains(got, "OpenTofu read the files of //app:app in this directory without any other target's files") {
			t.Errorf("%s: hint = %q, want one: %v", tc.name, got, tc.want)
		}
	}
}
