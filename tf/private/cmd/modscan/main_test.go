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
	})
	if err != nil {
		t.Fatal(err)
	}
	want := &Result{
		Providers: []string{"registry.opentofu.org/hashicorp/aws", "registry.opentofu.org/hashicorp/random"},
		Deps:      []string{"@tf_modules//acme/dns/aws"},
		Undeclared: []Call{

			{Source: "git::https://e.com/label.git?ref=v1", Where: "shared/main.tf:1"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestScan_Root(t *testing.T) {
	repo := writeTree(t, map[string]string{"main.tf": `output "x" { value = 1 }`})
	got, err := scan(repo, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Providers) != 0 || len(got.Deps) != 0 || len(got.Undeclared) != 0 {
		t.Errorf("got %+v, want nothing", got)
	}
}

func TestScan_NoConfig(t *testing.T) {
	repo := writeTree(t, map[string]string{"README.md": ``})
	_, err := scan(repo, "vpc", nil)
	if err == nil || !strings.Contains(err.Error(), `"/vpc" holds no .tf`) {
		t.Errorf("got %v", err)
	}
}
