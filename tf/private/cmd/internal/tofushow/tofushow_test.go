package tofushow_test

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/benbirt/rules_tofu/tf/private/cmd/internal/tofushow"
	"github.com/benbirt/rules_tofu/tf/private/cmd/internal/tofushow/tofutest"
)

var tofu = flag.String("tofu", "", "path to the tofu binary to read modules with")

func reader(t *testing.T) tofushow.Reader {
	t.Helper()
	return tofutest.Reader(t, *tofu)
}

// glob lists every regular file in dir under root, including dotfiles and
// non-configuration files that OpenTofu ignores.
func glob(t *testing.T, root, dir string) []string {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(dir))
	entries, err := os.ReadDir(abs)
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() {
			files = append(files, filepath.Join(abs, e.Name()))
		}
	}
	return files
}

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// Checks the `tofu show -json -module` behaviour that rules_tofu relies on.
// The output carries no format_version, so this test guards against format
// changes across OpenTofu releases.
func TestReadFiles_Facts(t *testing.T) {
	root := writeTree(t, map[string]string{
		"app/main.tf": `
terraform {
  required_providers {
    random = {
      source  = "HashiCorp/random"
      version = ">= 3.0"
    }
    corp = {
      source = "example.com/acme/corp"
    }
  }
}

provider "google" {
  alias = "eu"
}

provider "google" {
  alias = "us"
}

resource "null_resource" "implied" {}

resource "terraform_data" "builtin" {}

data "http" "implied" {
  url = "https://example.com"
}

variable "where" {
  default = "./evaluated"
}

module "zeta" {
  source = var.where
}

module "registry" {
  source  = "Acme/Network/aws//modules/subnet"
  version = "~> 1.2"
}

module "overridden" {
  source = "./before"
}

module "shadowed" {
  source = "./shadowed"
}
`,
		"app/main_override.tf":   `module "overridden" { source = "./after" }`,
		"app/shadow.tf":          `module "tf" { source = "./tf" }`,
		"app/shadow.tofu":        `module "tofu" { source = "./tofu" }`,
		"app/extra.tf.json":      `{"module": {"json": {"source": "git::https://example.com/x.git//sub?ref=v1"}}}`,
		"app/.hidden.tf":         `not configuration`,
		"app/main.tftest.hcl":    "mock_provider \"aws\" {}\nrun \"r\" {\n  module {\n    source = \"./helper\"\n  }\n}\n",
		"app/unrelated/other.tf": `module "other" { source = "./other" }`,
	})
	got, err := reader(t).ReadFiles("app", glob(t, root, "app"))
	if err != nil {
		t.Fatal(err)
	}
	want := &tofushow.Module{
		Calls: []tofushow.Call{
			{Name: "json", Source: "git::https://example.com/x.git//sub?ref=v1"},
			{Name: "overridden", Source: "./after"},
			{Name: "registry", Source: "Acme/Network/aws//modules/subnet", Version: "~> 1.2"},
			{Name: "shadowed", Source: "./shadowed"},
			{Name: "tofu", Source: "./tofu"},
			{Name: "zeta", Source: "./evaluated"},
		},
		Providers: []tofushow.Provider{
			{Source: "example.com/acme/corp"},
			{Source: "registry.opentofu.org/hashicorp/google"},
			{Source: "registry.opentofu.org/hashicorp/http"},
			{Source: "registry.opentofu.org/hashicorp/null"},
			{Source: "registry.opentofu.org/hashicorp/random", Version: ">= 3.0.0"},
			{Source: "terraform.io/builtin/terraform"},
		},
		LocalNames: map[string]string{
			"corp":      "example.com/acme/corp",
			"google":    "registry.opentofu.org/hashicorp/google",
			"http":      "registry.opentofu.org/hashicorp/http",
			"null":      "registry.opentofu.org/hashicorp/null",
			"random":    "registry.opentofu.org/hashicorp/random",
			"terraform": "terraform.io/builtin/terraform",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got\n%+v\nwant\n%+v", got, want)
	}
}

func TestReadFiles_RootAndEmpty(t *testing.T) {
	root := writeTree(t, map[string]string{
		"main.tf":       `module "a" { source = "./a" }`,
		"empty/README":  ``,
		"partial/x.tf":  "resource \"null_resource\" \"x\" {\n  triggers = {\n    a = local.elsewhere\n    b = var.undeclared\n  }\n}\n",
		"partial/y.txt": ``,
	})
	r := reader(t)
	got, err := r.ReadFiles("", glob(t, root, ""))
	if err != nil {
		t.Fatal(err)
	}
	if want := []tofushow.Call{{Name: "a", Source: "./a"}}; !reflect.DeepEqual(got.Calls, want) {
		t.Errorf("root: got %+v, want %+v", got.Calls, want)
	}
	got, err = r.ReadFiles("empty", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Calls) != 0 || len(got.Providers) != 0 {
		t.Errorf("empty directory: got %+v", got)
	}
	// Staging a subset of a directory's files still loads when resource
	// bodies reference locals or variables declared in another target's
	// files: `tofu show -module` does not evaluate resource arguments.
	got, err = r.ReadFiles("partial", glob(t, root, "partial"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []tofushow.Provider{{Source: "registry.opentofu.org/hashicorp/null"}}; !reflect.DeepEqual(got.Providers, want) {
		t.Errorf("partial module: got %+v, want %+v", got.Providers, want)
	}
}

func TestReadFiles_Error(t *testing.T) {
	root := writeTree(t, map[string]string{
		"broken/main.tf": "module \"m\" {\n  source = var.undeclared\n}\n",
	})
	_, err := reader(t).ReadFiles("pkg/broken", glob(t, root, "broken"))
	var load *tofushow.LoadError
	if !errors.As(err, &load) {
		t.Fatalf("got %v, want a *LoadError", err)
	}
	if want := "on pkg/broken/main.tf line 2, in module \"m\":"; load.Dir != "pkg/broken" || !strings.Contains(load.Diagnostics, want) {
		t.Errorf("got %+v, want diagnostics containing %q", load, want)
	}
	if prefix := "tofu cannot load the module in \"pkg/broken\":\n\nError: "; !strings.HasPrefix(err.Error(), prefix) {
		t.Errorf("error %q does not start with %q", err, prefix)
	}
}

// TF_* variables and HOME are isolated, so environment variables, logging and
// user CLI configuration cannot affect what `tofu show` reports.
func TestReadFiles_IgnoresEnvironment(t *testing.T) {
	root := writeTree(t, map[string]string{
		"main.tf": "variable \"where\" {\n  default = \"./default\"\n}\nmodule \"m\" {\n  source = var.where\n}\n",
	})
	r := reader(t)
	t.Setenv("TF_VAR_where", "./from-environment")
	t.Setenv("TF_CLI_ARGS_show", "-var where=./from-arguments")
	t.Setenv("TF_LOG", "TRACE")
	t.Setenv("TF_CLI_CONFIG_FILE", filepath.Join(root, "missing.tofurc"))
	home := writeTree(t, map[string]string{".tofurc": "not CLI configuration {"})
	t.Setenv("HOME", home)
	got, err := r.ReadFiles("", glob(t, root, ""))
	if err != nil {
		t.Fatal(err)
	}
	if want := []tofushow.Call{{Name: "m", Source: "./default"}}; !reflect.DeepEqual(got.Calls, want) {
		t.Errorf("got %+v, want %+v", got.Calls, want)
	}
}

// ReadFiles stages only the listed files under dir, ignoring sibling files in
// their source directory.
func TestReadFiles(t *testing.T) {
	root := writeTree(t, map[string]string{
		"src/main.tf":          "module \"mine\" {\n  source = \"./mine\"\n}\nresource \"null_resource\" \"x\" {}\n",
		"src/theirs.tf":        "module \"theirs\" {\n  source = \"./theirs\"\n}\nresource \"random_id\" \"x\" {}\n",
		"src/main_override.tf": `module "mine" { source = "./overridden" }`,
		"src/main.tofu":        `module "tofu" { source = "./tofu" }`,
	})
	file := func(name string) string { return filepath.Join(root, "src", name) }
	r := reader(t)

	got, err := r.ReadFiles("pkg/app", []string{file("main.tf")})
	if err != nil {
		t.Fatal(err)
	}
	want := &tofushow.Module{
		Calls:      []tofushow.Call{{Name: "mine", Source: "./mine"}},
		Providers:  []tofushow.Provider{{Source: "registry.opentofu.org/hashicorp/null"}},
		LocalNames: map[string]string{"null": "registry.opentofu.org/hashicorp/null"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("one file: got %+v, want %+v", got, want)
	}

	got, err = r.ReadFiles("", []string{file("main.tf"), file("main_override.tf")})
	if err != nil {
		t.Fatal(err)
	}
	if want := []tofushow.Call{{Name: "mine", Source: "./overridden"}}; !reflect.DeepEqual(got.Calls, want) {
		t.Errorf("with its override file: got %+v, want %+v", got.Calls, want)
	}

	got, err = r.ReadFiles("pkg/app", []string{file("main.tf"), file("main.tofu")})
	if err != nil {
		t.Fatal(err)
	}
	if want := []tofushow.Call{{Name: "tofu", Source: "./tofu"}}; !reflect.DeepEqual(got.Calls, want) {
		t.Errorf("with the .tofu file that shadows it: got %+v, want %+v", got.Calls, want)
	}

	_, err = r.ReadFiles("pkg/app", []string{file("main_override.tf")})
	var load *tofushow.LoadError
	if !errors.As(err, &load) || !strings.Contains(load.Diagnostics, "on pkg/app/main_override.tf line 1:") {
		t.Errorf("an override file alone: got %v, want a *LoadError naming pkg/app/main_override.tf", err)
	}
}

func TestParse(t *testing.T) {
	got, err := tofushow.Parse([]byte(`{
		"provider_config": {
			"aws": {"name": "aws", "full_name": "registry.opentofu.org/hashicorp/aws"},
			"aws.west": {"name": "aws", "full_name": "registry.opentofu.org/hashicorp/aws", "alias": "west", "version_constraint": "~> 5.0"}
		},
		"root_module": {"future_property": 1}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := []tofushow.Provider{{Source: "registry.opentofu.org/hashicorp/aws", Version: "~> 5.0"}}; !reflect.DeepEqual(got.Providers, want) {
		t.Errorf("got %+v, want %+v", got.Providers, want)
	}
	for name, output := range map[string]string{
		"not JSON":                         `Error: something`,
		"no root_module":                   `{"format_version": "2.0"}`,
		"a call without a source":          `{"root_module": {"module_calls": {"m": {}}}}`,
		"a provider without a full_name":   `{"provider_config": {"aws": {"name": "aws"}}, "root_module": {}}`,
		"a provider without a name":        `{"provider_config": {"aws": {"full_name": "example.com/a/aws"}}, "root_module": {}}`,
		"one local name for two providers": `{"provider_config": {"aws": {"name": "aws", "full_name": "example.com/a/aws"}, "aws.west": {"name": "aws", "full_name": "example.com/b/aws"}}, "root_module": {}}`,
		"two constraints for one provider": `{"provider_config": {"aws": {"name": "aws", "full_name": "example.com/a/aws", "version_constraint": "1.0.0"}, "aws.west": {"name": "aws", "full_name": "example.com/a/aws", "version_constraint": "2.0.0"}}, "root_module": {}}`,
	} {
		if _, err := tofushow.Parse([]byte(output)); err == nil || !strings.Contains(err.Error(), "unexpected output") {
			t.Errorf("%s: got %v, want an \"unexpected output\" error", name, err)
		}
	}
}

// Two local names for one provider share the constraint OpenTofu combines from
// both entries, and a local name no staged file declares resolves to
// hashicorp/<name>.
func TestReadFiles_LocalNames(t *testing.T) {
	root := writeTree(t, map[string]string{
		"versions.tf": `
terraform {
  required_providers {
    first = {
      source  = "hashicorp/aws"
      version = ">= 5.0"
    }
    second = {
      source  = "hashicorp/aws"
      version = "< 6.0"
    }
    posthog = {
      source = "PostHog/posthog"
    }
  }
}
`,
		"main.tf": "provider \"posthog\" {}\n",
	})
	r := reader(t)
	got, err := r.ReadFiles("app", glob(t, root, ""))
	if err != nil {
		t.Fatal(err)
	}
	want := &tofushow.Module{
		Providers: []tofushow.Provider{
			{Source: "registry.opentofu.org/hashicorp/aws", Version: ">= 5.0.0, < 6.0.0"},
			{Source: "registry.opentofu.org/posthog/posthog"},
		},
		LocalNames: map[string]string{
			"aws":     "registry.opentofu.org/hashicorp/aws",
			"first":   "registry.opentofu.org/hashicorp/aws",
			"second":  "registry.opentofu.org/hashicorp/aws",
			"posthog": "registry.opentofu.org/posthog/posthog",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("whole module: got\n%+v\nwant\n%+v", got, want)
	}

	got, err = r.ReadFiles("app", []string{filepath.Join(root, "main.tf")})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"posthog": "registry.opentofu.org/hashicorp/posthog"}; !reflect.DeepEqual(got.LocalNames, want) {
		t.Errorf("without versions.tf: got %+v, want %+v", got.LocalNames, want)
	}
}

func TestReadFiles_RejectsBadArguments(t *testing.T) {
	root := writeTree(t, map[string]string{"main.tf": ``, "terraform.tfvars": ``, "x.auto.tfvars.json": `{}`})
	r := reader(t)
	for _, dir := range []string{"/abs", "..", "../up", "a/../b", "a//b", "a/"} {
		if _, err := r.ReadFiles(dir, []string{filepath.Join(root, "main.tf")}); err == nil || !strings.Contains(err.Error(), "not a clean relative path") {
			t.Errorf("dir %q: got %v, want an error", dir, err)
		}
	}
	for _, name := range []string{"terraform.tfvars", "x.auto.tfvars.json"} {
		if _, err := r.ReadFiles("", []string{filepath.Join(root, name)}); err == nil || !strings.Contains(err.Error(), "is a variables file") {
			t.Errorf("%s: got %v, want an error", name, err)
		}
	}
}
