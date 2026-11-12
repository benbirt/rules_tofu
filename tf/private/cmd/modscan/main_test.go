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
	}, "")
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
	got, err := scan(repo, "", nil, "")
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
	got, err := scan(repo, "vpc", modules, self)
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
	}, self)
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
	_, err := scan(repo, "vpc", nil, "")
	if err == nil || !strings.Contains(err.Error(), `"/vpc" holds no .tf`) {
		t.Errorf("got %v", err)
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
