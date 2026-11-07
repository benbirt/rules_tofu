package tfconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestNormalizeSource(t *testing.T) {
	for in, want := range map[string]string{
		"Acme/Net/AWS":                              "registry.opentofu.org/acme/net/aws",
		"acme/net/aws//Modules/X":                   "registry.opentofu.org/acme/net/aws//Modules/X",
		"Registry.Terraform.io/acme/net/aws":        "registry.terraform.io/acme/net/aws",
		"github.com/acme/net":                       "github.com/acme/net",
		"github.com/acme/net/aws":                   "github.com/acme/net/aws",
		"github.com/a/b/c":                          "github.com/a/b/c",
		"git::https://example.com/x.git//d?ref=v1":  "git::https://example.com/x.git//d?ref=v1",
		"https://example.com/x.tar.gz":              "https://example.com/x.tar.gz",
		"./local":                                   "./local",
		"a/b":                                       "a/b",
		"s3::https://s3.amazonaws.com/bucket/x.zip": "s3::https://s3.amazonaws.com/bucket/x.zip",
	} {
		if got := NormalizeSource(in); got != want {
			t.Errorf("NormalizeSource(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitSubdir(t *testing.T) {
	for in, want := range map[string][2]string{
		"acme/net/aws":                           {"acme/net/aws", ""},
		"acme/net/aws//modules/x":                {"acme/net/aws", "modules/x"},
		"git::https://e.com/x.git//vpc?ref=v1.0": {"git::https://e.com/x.git?ref=v1.0", "vpc"},
		"https://e.com/x.tar.gz//sub/":           {"https://e.com/x.tar.gz", "sub"},
		"https://e.com/x.tar.gz":                 {"https://e.com/x.tar.gz", ""},
	} {
		base, subdir := SplitSubdir(in)
		if base != want[0] || subdir != want[1] {
			t.Errorf("SplitSubdir(%q) = (%q, %q), want %q", in, base, subdir, want)
		}
	}
}

func TestMatch(t *testing.T) {
	for _, tc := range []struct {
		provider, source, subdir string
		ok                       bool
	}{
		{"acme/net/aws", "Acme/Net/AWS", "", true},
		{"acme/net/aws", "acme/net/aws//modules/x", "modules/x", true},
		{"acme/net/aws//modules", "acme/net/aws//modules/x", "x", true},
		{"acme/net/aws//modules/x", "acme/net/aws//modules", "", false},
		{"acme/net/aws//mod", "acme/net/aws//modules", "", false},
		{"git::https://e.com/r.git//vpc?ref=v1", "git::https://e.com/r.git//vpc/sub?ref=v1", "sub", true},
		{"git::https://e.com/r.git?ref=v1", "git::https://e.com/r.git//vpc?ref=v1", "vpc", true},
		{"git::https://e.com/r.git?ref=v1", "git::https://e.com/r.git?ref=v2", "", false},
		{"acme/net/aws", "acme/other/aws", "", false},
	} {
		subdir, ok := Match(tc.provider, tc.source)
		if subdir != tc.subdir || ok != tc.ok {
			t.Errorf("Match(%q, %q) = (%q, %v), want (%q, %v)", tc.provider, tc.source, subdir, ok, tc.subdir, tc.ok)
		}
	}
}

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
