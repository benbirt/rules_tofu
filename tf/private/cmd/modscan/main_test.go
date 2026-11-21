package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/benbirt/rules_tofu/tf/private/cmd/internal/tofushow"
	"github.com/benbirt/rules_tofu/tf/private/cmd/internal/tofushow/tofutest"
)

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

func TestScan(t *testing.T) {
	repo := writeTree(t, map[string]string{
		"vpc/main.tf": `
terraform {
  required_providers {
    aws = { source = "hashicorp/aws" }
  }
}
module "shared" { source = "../shared" }
module "dns" {
  source  = "acme/dns/aws"
  version = "~> 1.0"
}
`,
		"shared/main.tf":                `module "label" { source = "git::https://e.com/label.git?ref=v1" }`,
		"vpc/modules/endpoints/main.tf": "terraform {\n  required_providers {\n    random = {}\n  }\n}\n",
		"vpc/modules/README.md":         ``,
		"vpc/examples/complete/main.tf": `module "x" { source = "acme/unused/aws" }
terraform {
  required_providers {
    tls = {}
  }
}`,
		"unrelated/main.tf": `module "y" { source = "acme/other/aws" }`,
	})
	got, err := scan(tofutest.Reader(t, *tofu), repo, "vpc", []Module{
		{Label: "@tf_modules//acme/dns/aws", Source: "Acme/DNS/aws"},
		{Label: "@tf_modules//unused", Source: "git::https://e.com/unused.git"},
	}, "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := &Result{
		Dirs:      []string{"shared", "vpc", "vpc/modules/endpoints"},
		Providers: []string{"registry.opentofu.org/hashicorp/aws", "registry.opentofu.org/hashicorp/random"},
		Deps:      []string{"@tf_modules//acme/dns/aws"},
		Undeclared: []Call{

			{Source: "git::https://e.com/label.git?ref=v1", Where: `module "label" in "shared"`, Tag: `tf_modules.module(name = "...", source = "git::https://e.com/label.git?ref=v1")`},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// Providers come from OpenTofu for the served directories: override files
// replace primary entries, resources and data sources without a
// required_providers entry imply hashicorp/<prefix>, and built-in providers
// under both terraform.io/builtin/ and opentofu.org/builtin/ are omitted.
func TestScan_Providers(t *testing.T) {
	requires := func(name, source string) string {
		return "terraform {\n  required_providers {\n    " + name + " = { source = \"" + source + "\" }\n  }\n}\n"
	}
	repo := writeTree(t, map[string]string{
		"a.tf":          requires("null", "hashicorp/null"),
		"c_override.tf": requires("null", "Acme/Null"),
		"main.tf": `
resource "random_id" "implied" {}
data "http" "implied" { url = "https://example.com" }
resource "terraform_data" "builtin" {}
`,
		// OpenTofu reports this one as opentofu.org/builtin/terraform.
		"modules/renamed/main.tf": requires("tf", "opentofu.org/builtin/terraform") + "resource \"terraform_data\" \"builtin\" {\n  provider = tf\n}\n",
	})
	got, err := scan(tofutest.Reader(t, *tofu), repo, "", nil, "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"registry.opentofu.org/acme/null",
		"registry.opentofu.org/hashicorp/http",
		"registry.opentofu.org/hashicorp/random",
	}
	if !reflect.DeepEqual(got.Providers, want) {
		t.Errorf("got %v, want %v", got.Providers, want)
	}
}

// Module calls come from OpenTofu: sources built from local values or variable
// defaults work for local paths, self-calls and external modules; override
// files replace primary sources; and .tofu files shadow .tf files of the same
// name. Auto-loaded .tfvars files in the fetched tree and TF_VAR_* in the
// environment have no effect.
func TestScan_SourcesFromTofu(t *testing.T) {
	const self = "@tf_modules//net"
	modules := []Module{
		{Label: self, Source: "git::https://e.com/net.git?ref=v1"},
		{Label: "@tf_modules//acme/dns/aws", Source: "acme/dns/aws"},
	}
	repo := writeTree(t, map[string]string{
		"main.tf": `
variable "dir" { default = "./local" }
variable "ref" { default = "v1" }
locals { net = "git::https://e.com/net.git" }
module "local" { source = var.dir }
module "own" { source = "${local.net}//own?ref=${var.ref}" }
module "replaced" { source = "acme/unused/aws" }
`,
		"main_override.tf":     `module "replaced" { source = "acme/dns/aws" }`,
		"shadowed.tf":          `module "shadowed" { source = "acme/shadowed/aws" }`,
		"shadowed.tofu":        ``,
		"terraform.tfvars":     `dir = "./elsewhere"`,
		"local/main.tf":        ``,
		"own/main.tf":          ``,
		"elsewhere/main.tf":    `module "x" { source = "acme/elsewhere/aws" }`,
		"examples/own/main.tf": `module "x" { source = "acme/example/aws" }`,
	})
	t.Setenv("TF_VAR_dir", "./elsewhere")
	t.Setenv("TF_VAR_ref", "v2")
	got, err := scan(tofutest.Reader(t, *tofu), repo, "", modules, self, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := &Result{
		Dirs:       []string{"", "local", "own"},
		Providers:  []string{},
		Deps:       []string{"@tf_modules//acme/dns/aws"},
		Undeclared: []Call{},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// A served directory that OpenTofu cannot load fails the scan with OpenTofu's
// diagnostics, naming files relative to the fetched tree. This covers checks
// only OpenTofu performs, such as a source referencing a variable with no
// default or duplicate required_providers local names. Unserved directories are
// not read.
func TestScan_LoadError(t *testing.T) {
	for name, tc := range map[string]struct {
		files map[string]string
		want  []string
	}{
		"a source that has no value": {
			files: map[string]string{"vpc/modules/sub/main.tf": "variable \"dir\" {}\nmodule \"a\" {\n  source = var.dir\n}\n"},
			want:  []string{`tofu cannot load the module in "vpc/modules/sub"`, "on vpc/modules/sub/main.tf line 3"},
		},
		"a duplicate required_providers entry": {
			files: map[string]string{
				"vpc/a.tf": "terraform {\n  required_providers {\n    x = { source = \"hashicorp/aws\" }\n  }\n}\n",
				"vpc/b.tf": "terraform {\n  required_providers {\n    x = { source = \"hashicorp/random\" }\n  }\n}\n",
			},
			want: []string{`tofu cannot load the module in "vpc"`, "vpc/b.tf"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			files := map[string]string{
				"vpc/main.tf":               ``,
				"vpc/examples/bad/main.tf":  `module "broken" {`,
				"vpc/modules/README.md":     ``,
				"unrelated/modules/main.tf": `module "broken" {`,
			}
			for name, content := range tc.files {
				files[name] = content
			}
			_, err := scan(tofutest.Reader(t, *tofu), writeTree(t, files), "vpc", nil, "", nil, nil)
			var load *tofushow.LoadError
			if !errors.As(err, &load) {
				t.Fatalf("got %v, want a *tofushow.LoadError", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("got %v, want an error containing %q", err, want)
				}
			}
		})
	}
}

func TestScan_Root(t *testing.T) {
	repo := writeTree(t, map[string]string{"main.tf": `output "x" { value = 1 }`})
	got, err := scan(tofutest.Reader(t, *tofu), repo, "", nil, "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Providers) != 0 || len(got.Deps) != 0 || len(got.Undeclared) != 0 {
		t.Errorf("got %+v, want nothing", got)
	}
}

// A module may call its own address with `//<subdir>`: it provides that
// itself, with no dep, and the directory is part of what its library serves,
// like one a local path reaches (#95).
func TestScan_SelfCall(t *testing.T) {
	const self = "@tf_modules//infra"
	modules := []Module{
		{Label: self, Source: "git::https://e.com/infra.git//vpc?ref=v1"},
		{Label: "@tf_modules//acme/dns/aws", Source: "acme/dns/aws"},
		{Label: "@tf_modules//label", Source: "git::https://e.com/label.git?ref=v1"},
	}
	repo := writeTree(t, map[string]string{
		"vpc/main.tf": `
module "peering" { source = "git::https://e.com/infra.git//vpc/peering?ref=v1" }
module "me" { source = "git::https://e.com/infra.git//vpc?ref=v1" }
module "nothing" { source = "git::https://e.com/infra.git//vpc/docs?ref=v1" }
`,
		// Outside `modules/`, so reached only through the call above; it
		// calls another of the module's directories in turn, and back.
		"vpc/peering/main.tf": `
terraform {
  required_providers {
    aws = { source = "hashicorp/aws" }
  }
}
module "routes" { source = "git::https://e.com/infra.git//vpc/routes?ref=v1" }
module "dns" { source = "acme/dns/aws" }
`,
		"vpc/routes/main.tf": `
terraform {
  required_providers {
    random = {}
  }
}
module "back" { source = "git::https://e.com/infra.git//vpc/peering?ref=v1" }
module "label" { source = "git::https://e.com/label.git?ref=v1" }
`,
		"vpc/docs/README.md":          ``,
		"vpc/examples/demo/main.tf":   `module "x" { source = "acme/unused/aws" }`,
		"vpc/examples/demo/extras.tf": "terraform {\n  required_providers {\n    tls = {}\n  }\n}\n",
	})
	got, err := scan(tofutest.Reader(t, *tofu), repo, "vpc", modules, self, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := &Result{
		Dirs:       []string{"vpc", "vpc/peering", "vpc/routes"},
		Providers:  []string{"registry.opentofu.org/hashicorp/aws", "registry.opentofu.org/hashicorp/random"},
		Deps:       []string{"@tf_modules//acme/dns/aws", "@tf_modules//label"},
		Undeclared: []Call{},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// When another tag declares one of the module's subdirectories, that one
// provides the calls into it, as it will in the modules manifest (#97): a
// dep, and its files aren't this library's to describe.
func TestScan_SelfCallMoreSpecificModule(t *testing.T) {
	const self = "@tf_modules//acme/net/aws"
	repo := writeTree(t, map[string]string{
		"main.tf": `
module "a" { source = "acme/net/aws//parts/a" }
module "b" { source = "acme/net/aws//parts/b" }
`,
		"parts/a/main.tf": "terraform {\n  required_providers {\n    tls = {}\n  }\n}\n",
		"parts/b/main.tf": "terraform {\n  required_providers {\n    random = {}\n  }\n}\n",
	})
	got, err := scan(tofutest.Reader(t, *tofu), repo, "", []Module{
		// The longer string, and the less specific address.
		{Label: self, Source: "registry.opentofu.org/acme/net/aws"},
		{Label: "@tf_modules//a", Source: "acme/net/aws//parts/a"},
	}, self, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := &Result{
		Dirs:       []string{"", "parts/b"},
		Providers:  []string{"registry.opentofu.org/hashicorp/random"},
		Deps:       []string{"@tf_modules//a"},
		Undeclared: []Call{},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestScan_NoConfig(t *testing.T) {
	repo := writeTree(t, map[string]string{"README.md": ``})
	_, err := scan(tofutest.Reader(t, *tofu), repo, "vpc", nil, "", nil, nil)
	if err == nil || !strings.Contains(err.Error(), `"/vpc" holds no .tf`) {
		t.Errorf("got %v", err)
	}
}

// A git submodule of the fetched commit is an empty directory: the fetch
// leaves it out. A module that is in one, or whose own calls lead into one,
// fails here, naming the submodule; one that merely has them does not.
func TestScan_GitSubmodule(t *testing.T) {
	const self = "@tf_modules//net"
	modules := []Module{{Label: self, Source: "git::https://e.com/net.git"}}
	submodules := []string{"vendor/shared", "docs/theme/"}
	tree := func(t *testing.T, mainTF string) string {
		repo := writeTree(t, map[string]string{
			"main.tf":     mainTF,
			"vpc/main.tf": ``,
		})
		// What a checkout makes of a gitlink.
		for _, dir := range submodules {
			if err := os.MkdirAll(filepath.Join(repo, filepath.FromSlash(dir)), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		return repo
	}

	t.Run("an uncalled submodule is nobody's business", func(t *testing.T) {
		repo := tree(t, `module "vpc" { source = "./vpc" }`)
		if _, err := scan(tofutest.Reader(t, *tofu), repo, "", modules, self, nil, submodules); err != nil {
			t.Fatal(err)
		}
		// Nor is a sibling whose name only starts like the submodule's.
		repo = writeTree(t, map[string]string{"vendor/shared-not/main.tf": ``})
		if _, err := scan(tofutest.Reader(t, *tofu), repo, "vendor/shared-not", modules, self, nil, submodules); err != nil {
			t.Fatal(err)
		}
	})

	for name, tc := range map[string]struct {
		moduleDir, mainTF, want string
	}{
		"the module's own directory is one": {
			moduleDir: "vendor/shared",
			want:      `"/vendor/shared" is in the git submodule "vendor/shared" of the repository, and rules_tofu does not fetch git submodules`,
		},
		"the module's own directory is inside one": {
			moduleDir: "/docs/theme/tf/",
			want:      `"/docs/theme/tf" is in the git submodule "docs/theme" of the repository`,
		},
		"a local call leads to one": {
			mainTF: "\nmodule \"shared\" { source = \"./vendor/shared\" }",
			want:   `module "shared" in ".": source "./vendor/shared" leads into the git submodule "vendor/shared", and rules_tofu does not fetch git submodules`,
		},
		"a call by its own address leads to one": {
			mainTF: `module "shared" { source = "git::https://e.com/net.git//vendor/shared" }`,
			want:   `module "shared" in ".": source "git::https://e.com/net.git//vendor/shared" leads into the git submodule "vendor/shared"`,
		},
		"a call by its own address leads inside one": {
			mainTF: `module "shared" { source = "git::https://e.com/net.git//vendor/shared/nested" }`,
			want:   `leads into the git submodule "vendor/shared"`,
		},
		// The directory below the submodule's doesn't exist at all, which
		// is an error of ModuleDirs's own; it gains the reason.
		"a local call leads inside one": {
			mainTF: `module "shared" { source = "./vendor/shared/nested" }`,
			want:   `(it is in the git submodule "vendor/shared", and rules_tofu does not fetch git submodules (OpenTofu does))`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			repo := tree(t, tc.mainTF)
			_, err := scan(tofutest.Reader(t, *tofu), repo, tc.moduleDir, modules, self, nil, submodules)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want an error containing %q", err, tc.want)
			}
			if err != nil && strings.Contains(err.Error(), "docs/theme") && !strings.Contains(tc.want, "docs/theme") {
				t.Errorf("the error names a submodule it isn't about: %v", err)
			}
		})
	}

	// An error that has nothing to do with a submodule says nothing of
	// them, though the repository has some.
	for name, tc := range map[string]struct {
		moduleDir, mainTF, want string
	}{
		"a local call to a directory that isn't there": {
			mainTF: `module "gone" { source = "./gone" }`,
			want:   "gone",
		},
		"a module directory without configuration": {
			moduleDir: "vendor",
			want:      `"/vendor" holds no .tf`,
		},
		"configuration that can't be read": {
			mainTF: `module "broken" {`,
			want:   "main.tf",
		},
	} {
		t.Run(name, func(t *testing.T) {
			repo := tree(t, tc.mainTF)
			_, err := scan(tofutest.Reader(t, *tofu), repo, tc.moduleDir, modules, self, nil, submodules)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "submodule") {
				t.Errorf("the error blames a git submodule: %v", err)
			}
		})
	}
}

func TestTagFor(t *testing.T) {
	for in, want := range map[string]string{
		"acme/dns/aws//modules/zone":  `tf_modules.module(source = "acme/dns/aws", version = "...")`,
		"git::https://e.com/x.git//d": `tf_modules.module(name = "...", source = "git::https://e.com/x.git//d")`,
	} {
		if got := tagFor(in); got != want {
			t.Errorf("tagFor(%q) = %s, want %s", in, got, want)
		}
	}
}

// A directory named by --subdir is served like one under modules/: its
// providers and calls are the library's, and so is what it reaches by a
// local path. One that leaves the module or holds no configuration is an
// error, since a caller's build could not load it.
func TestScan_Subdir(t *testing.T) {
	repo := writeTree(t, map[string]string{
		"vpc/main.tf":         ``,
		"vpc/peering/main.tf": "terraform {\n  required_providers {\n    time = {}\n  }\n}\n" + `module "routes" { source = "../routes" }`,
		"vpc/routes/main.tf":  `module "dns" { source = "acme/dns/aws" }`,
		"vpc/docs/README.md":  ``,
		"other/main.tf":       ``,
	})
	modules := []Module{{Label: "@tf_modules//acme/dns/aws", Source: "acme/dns/aws"}}
	got, err := scan(tofutest.Reader(t, *tofu), repo, "vpc", modules, "", []string{"peering"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := &Result{
		Dirs:       []string{"vpc", "vpc/peering", "vpc/routes"},
		Providers:  []string{"registry.opentofu.org/hashicorp/time"},
		Deps:       []string{"@tf_modules//acme/dns/aws"},
		Undeclared: []Call{},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	for dir, fragment := range map[string]string{
		"docs":      `"docs" holds no .tf`,
		"absent":    `the module has no directory "absent"`,
		"//peering": `no leading`,
		`peer\ing`:  `forward slashes`,
		"../other":  `"../other" is not a directory below the module's own`,
		".":         `"." is not a directory below the module's own`,
		"/vpc":      `"/vpc" is not a directory below the module's own`,
	} {
		if _, err := scan(tofutest.Reader(t, *tofu), repo, "vpc", modules, "", []string{dir}, nil); err == nil || !strings.Contains(err.Error(), fragment) {
			t.Errorf("subdir %q: got %v, want an error containing %q", dir, err, fragment)
		}
	}
}
