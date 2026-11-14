package tfconfig

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRequiredProviders(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"versions.tf": `terraform {
  required_providers {
    aws    = { source = "HashiCorp/aws", version = ">= 5.0" }
    random = { version = "~> 3.0" }
    other  = { source = "example.com/acme/other" }
  }
}`,
		"more.tf.json": `{"terraform": [{"required_providers": [{"null": {"source": "hashicorp/null"}}]}]}`,
		"main.tf":      `resource "aws_s3_bucket" "b" {}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := RequiredProviders(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"example.com/acme/other",
		"registry.opentofu.org/hashicorp/aws",
		"registry.opentofu.org/hashicorp/null",
		"registry.opentofu.org/hashicorp/random",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestReadRequiredProviders(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"main.tf": `
terraform {
  required_version = ">= 1.6"
  required_providers {
    aws = {
      source                = "hashicorp/aws"
      version               = ">= 6.0"
      configuration_aliases = [aws.east]
    }
    google = "~> 7.0" # legacy form: source implied
    ph = { source = "PostHog/posthog" }
  }
}

resource "null_resource" "x" {
  triggers = { a = var.undeclared }
}
`,
		"extra.tf.json": `{"terraform": {"required_providers": {"random": {"source": "hashicorp/random", "version": "3.6.0"}}}}`,
		"x_override.tf": `terraform {
  required_providers {
    random = { source = "hashicorp/random", version = "3.7.0" }
  }
}
`,
		"unlisted.tf": `terraform {
  required_providers {
    null = { source = "hashicorp/null" }
  }
}
`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ReadRequiredProviders([]string{
		filepath.Join(dir, "x_override.tf"), filepath.Join(dir, "main.tf"), filepath.Join(dir, "extra.tf.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Requirement{
		"aws":    {Source: "registry.opentofu.org/hashicorp/aws", Version: ">= 6.0"},
		"google": {Source: "registry.opentofu.org/hashicorp/google", Version: "~> 7.0"},
		"ph":     {Source: "registry.opentofu.org/posthog/posthog"},
		"random": {Source: "registry.opentofu.org/hashicorp/random", Version: "3.7.0"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// ReadRequirements is ReadRequiredProviders without the merging: every entry
// as written, primary files first, with what pin needs to combine them itself
// when two root modules share a directory (issue #99).
func TestReadRequirements(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"b.tf": `terraform {
  required_providers {
    null   = { source = "hashicorp/null", version = ">= 3.0" }
    random = { source = "hashicorp/random" }
    aws    = "~> 5.0"
  }
}
`,
		"a.tf": `terraform {
  required_providers {
    null = { source = "hashicorp/null", version = "3.2.4" }
  }
}
`,
		"a_override.tf": `terraform {
  required_providers {
    null = { source = "acme/null" }
  }
}
`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ReadRequirements([]string{
		filepath.Join(dir, "a_override.tf"), filepath.Join(dir, "b.tf"), filepath.Join(dir, "a.tf"),
	})
	if err != nil {
		t.Fatal(err)
	}
	type entry struct {
		Name, Source, Version, File string
		Override                    bool
	}
	var entries []entry
	for _, d := range got {
		file, _, _ := strings.Cut(filepath.Base(d.Range), ":")
		entries = append(entries, entry{d.Name, d.Source, d.Version, file, d.Override})
	}
	want := []entry{
		{"null", "registry.opentofu.org/hashicorp/null", "3.2.4", "a.tf", false},
		{"null", "registry.opentofu.org/hashicorp/null", ">= 3.0", "b.tf", false},
		{"random", "registry.opentofu.org/hashicorp/random", "", "b.tf", false},
		{"aws", "registry.opentofu.org/hashicorp/aws", "~> 5.0", "b.tf", false},
		{"null", "registry.opentofu.org/acme/null", "", "a_override.tf", true},
	}
	if !reflect.DeepEqual(entries, want) {
		t.Errorf("got  %+v\nwant %+v", entries, want)
	}
}

func TestReadRequiredProviders_DuplicateLocalName(t *testing.T) {
	dir := t.TempDir()
	null := "terraform {\n  required_providers {\n    null = { source = \"hashicorp/null\" }\n  }\n}\n"
	for _, name := range []string{"a.tf", "b.tf"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(null), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, err := ReadRequiredProviders([]string{filepath.Join(dir, "a.tf"), filepath.Join(dir, "b.tf")})
	if err == nil || !strings.Contains(err.Error(), "already required") {
		t.Errorf("expected a duplicate error, got %v", err)
	}
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
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

// The lists pin reads come from a `bazel query`, not from a directory, and
// go through the same filters.
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

func TestReadCalls_Tofu(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		// Shadowed by main.tofu, so neither its call nor the duplicate counts.
		"main.tf":         `module "a" { source = "acme/tf/aws" }`,
		"main.tofu":       `module "a" { source = "acme/tofu/aws" }`,
		"b.tofu.json":     `{"module": {"b": {"source": "./b"}}}`,
		"c.tofu":          `module "c" { source = "./c" }`,
		"c_override.tofu": `module "c" { source = "./c2" }`,
	})
	calls, err := ReadCalls(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, c := range calls {
		got[c.Name] = c.Source
	}
	want := map[string]string{"a": "acme/tofu/aws", "b": "./b", "c": "./c2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// pin hands ReadRequirements, which ReadRequiredProviders reads through, the
// files Bazel lists, both halves of a shadowed pair included; the .tf one
// must not count.
func TestReadRequiredProviders_TofuShadowsTf(t *testing.T) {
	dir := t.TempDir()
	null := func(version string) string {
		return "terraform {\n  required_providers {\n    null = { source = \"hashicorp/null\", version = \"" + version + "\" }\n  }\n}\n"
	}
	writeFiles(t, dir, map[string]string{
		"versions.tf":   null("3.1.0"),
		"versions.tofu": null("3.2.0"),
	})
	got, err := ReadRequiredProviders([]string{filepath.Join(dir, "versions.tf"), filepath.Join(dir, "versions.tofu")})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Requirement{"null": {Source: "registry.opentofu.org/hashicorp/null", Version: "3.2.0"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// ModuleDirs lists the module's directory, the configured directories under
// its modules/, the called directories that hold configuration and what local
// calls reach, backslash paths included; a call out of the repository is
// ignored, and examples count only when called.
func TestModuleDirs(t *testing.T) {
	repo := t.TempDir()
	for name, content := range map[string]string{
		"main.tf":                  `module "a" { source = ".\\lib\\a" }`,
		"lib/a/main.tf":            `module "out" { source = "..\\..\\..\\outside" }`,
		"modules/sub/main.tf":      `module "ex" { source = "../../examples/called" }`,
		"modules/README.md":        ``,
		"examples/called/main.tf":  ``,
		"examples/ignored/main.tf": ``,
		"tools/main.tf":            `module "deep" { source = "./deep" }`,
		"tools/deep/main.tf":       ``,
		"docs/README.md":           ``,
	} {
		p := filepath.Join(repo, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ModuleDirs(repo, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"", "examples/called", "lib/a", "modules/sub"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	// Called directories are followed too; one without configuration is
	// skipped, and the module's own among them changes nothing.
	got, err = ModuleDirs(repo, "", "", "tools/", "docs", "examples/called")
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
	for name, content := range map[string]string{
		"bundle/README.md":           ``,
		"bundle/modules/dns/main.tf": `module "shared" { source = "../../shared" }`,
		"bundle/shared/main.tf":      ``,
		"bundle/vpc/main.tf":         `module "peer" { source = "../peering" }`,
		"bundle/peering/main.tf":     ``,
		"bundle/examples/main.tf":    ``,
		"bare/BUILD.bazel":           ``,
	} {
		p := filepath.Join(repo, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
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
			got, err := ModuleDirs(repo, tc.moduleDir, tc.called...)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.want) || (len(got) > 0 && !reflect.DeepEqual(got, tc.want)) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}

	// A module directory that isn't there is not a bundle: that is an error.
	if got, err := ModuleDirs(repo, "nowhere", "bundle/vpc"); !errors.Is(err, fs.ErrNotExist) {
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
