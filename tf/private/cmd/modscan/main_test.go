package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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
	got, err := scan(repo, "vpc", []Module{
		{Label: "@tf_modules//acme/dns/aws", Source: "Acme/DNS/aws"},
		{Label: "@tf_modules//unused", Source: "git::https://e.com/unused.git"},
	}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := &Result{
		Providers: []string{"registry.opentofu.org/hashicorp/aws", "registry.opentofu.org/hashicorp/random"},
		Deps:      []string{"@tf_modules//acme/dns/aws"},
		Undeclared: []Call{

			{Source: "git::https://e.com/label.git?ref=v1", Where: "shared/main.tf:1", Tag: `tf_modules.module(name = "...", source = "git::https://e.com/label.git?ref=v1")`},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestScan_Root(t *testing.T) {
	repo := writeTree(t, map[string]string{"main.tf": `output "x" { value = 1 }`})
	got, err := scan(repo, "", nil, "", nil)
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
	got, err := scan(repo, "vpc", modules, self, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := &Result{
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
	got, err := scan(repo, "", []Module{
		// The longer string, and the less specific address.
		{Label: self, Source: "registry.opentofu.org/acme/net/aws"},
		{Label: "@tf_modules//a", Source: "acme/net/aws//parts/a"},
	}, self, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := &Result{
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
	_, err := scan(repo, "vpc", nil, "", nil)
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
		if _, err := scan(repo, "", modules, self, submodules); err != nil {
			t.Fatal(err)
		}
		// Nor is a sibling whose name only starts like the submodule's.
		repo = writeTree(t, map[string]string{"vendor/shared-not/main.tf": ``})
		if _, err := scan(repo, "vendor/shared-not", modules, self, submodules); err != nil {
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
			want:   `main.tf:2: module source "./vendor/shared" leads into the git submodule "vendor/shared", and rules_tofu does not fetch git submodules`,
		},
		"a call by its own address leads to one": {
			mainTF: `module "shared" { source = "git::https://e.com/net.git//vendor/shared" }`,
			want:   `main.tf:1: module source "git::https://e.com/net.git//vendor/shared" leads into the git submodule "vendor/shared"`,
		},
		"a call by its own address leads inside one": {
			mainTF: `module "shared" { source = "git::https://e.com/net.git//vendor/shared/nested" }`,
			want:   `leads into the git submodule "vendor/shared"`,
		},
		// The directory below the submodule's doesn't exist at all, which
		// is an error of ModuleDirs's own; it gains the reason.
		"a local call leads inside one": {
			mainTF: `module "shared" { source = "./vendor/shared/nested" }`,
			want:   `(the repository has git submodules, and rules_tofu does not fetch git submodules (OpenTofu does): vendor/shared, docs/theme)`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			repo := tree(t, tc.mainTF)
			_, err := scan(repo, tc.moduleDir, modules, self, submodules)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want an error containing %q", err, tc.want)
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
