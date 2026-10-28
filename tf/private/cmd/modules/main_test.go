package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeTree creates files (work-tree-relative path -> content) under a new
// work tree and returns its root.
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

func TestBuild(t *testing.T) {
	tree := writeTree(t, map[string]string{
		"app/main.tf": `
module "net" {
  source  = "Acme/Network/AWS"
  version = "~> 1.2"
}
module "sub" { source = "acme/network/aws//modules/subnet" }
module "git" { source = "git::https://example.com/infra.git//vpc?ref=v1.0.0" }
module "local" { source = "../shared" }
`,
		"shared/main.tf":                              `module "inner" { source = "./inner" }`,
		"shared/inner/main.tf":                        ``,
		"external/net+/main.tf":                       `module "helper" { source = "./helper" }`,
		"external/net+/helper/main.tf":                ``,
		"external/net+/modules/subnet/main.tf":        ``,
		"external/infra+/vpc/main.tf":                 `module "x" { source = "acme/dns/aws" }`,
		"external/dns+/main.tf":                       ``,
		"external/dns+/unrelated/ignored/main.tf.bak": ``,
	})
	deps := []dep{
		{Label: "@net//:net", Source: "acme/network/aws", Version: "1.2.3", Dir: "external/net+", Direct: true},
		{Label: "@infra//vpc", Source: "git::https://example.com/infra.git//vpc?ref=v1.0.0", Dir: "external/infra+/vpc", Direct: true},
		{Label: "@dns//:dns", Source: "registry.opentofu.org/acme/dns/aws", Version: "0.1.0", Dir: "external/dns+"},
	}
	got, err := build(tree, "app", "//app:app", deps)
	if err != nil {
		t.Fatal(err)
	}
	want := []Record{
		{Key: "", Source: "", Dir: "."},
		{Key: "git", Source: "git::https://example.com/infra.git//vpc?ref=v1.0.0", Dir: "../external/infra+/vpc"},
		{Key: "git.x", Source: "acme/dns/aws", Version: "0.1.0", Dir: "../external/dns+"},
		{Key: "local", Source: "../shared", Dir: "../shared"},
		{Key: "local.inner", Source: "./inner", Dir: "../shared/inner"},
		{Key: "net", Source: "Acme/Network/AWS", Version: "1.2.3", Dir: "../external/net+"},
		{Key: "net.helper", Source: "./helper", Dir: "../external/net+/helper"},
		{Key: "sub", Source: "acme/network/aws//modules/subnet", Version: "1.2.3", Dir: "../external/net+/modules/subnet"},
	}
	if !reflect.DeepEqual(got.Modules, want) {
		t.Errorf("got\n%+v\nwant\n%+v", got.Modules, want)
	}
}

func TestBuild_RootPackage(t *testing.T) {
	tree := writeTree(t, map[string]string{
		"main.tf":     `module "a" { source = "./a" }`,
		"a/main.tf":   ``,
		"versions.tf": `terraform {}`,
	})
	got, err := build(tree, "", "//:root", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []Record{{Key: "", Source: "", Dir: "."}, {Key: "a", Source: "./a", Dir: "a"}}
	if !reflect.DeepEqual(got.Modules, want) {
		t.Errorf("got %+v, want %+v", got.Modules, want)
	}
}

func TestBuild_JSONAndOverride(t *testing.T) {
	tree := writeTree(t, map[string]string{
		"app/main.tf.json":   `{"module": {"m": {"source": "./old"}}}`,
		"app/m_override.tf":  `module "m" { source = "./new" }`,
		"app/new/main.tf":    ``,
		"app/unused/main.tf": ``,
	})
	got, err := build(tree, "app", "//app", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []Record{{Key: "", Source: "", Dir: "."}, {Key: "m", Source: "./new", Dir: "new"}}
	if !reflect.DeepEqual(got.Modules, want) {
		t.Errorf("got %+v, want %+v", got.Modules, want)
	}
}

func TestBuild_Errors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		deps  []dep
		want  string
	}{
		{
			name:  "registry source with no dep",
			files: map[string]string{"app/main.tf": `module "vpc" { source = "terraform-aws-modules/vpc/aws//modules/x" }`},
			want:  `Declare it in MODULE.bazel with tf_modules.module(source = "terraform-aws-modules/vpc/aws", version = ...) and add "@tf_modules//terraform-aws-modules/vpc/aws" to deps`,
		},
		{
			name:  "git source with no dep",
			files: map[string]string{"app/main.tf": `module "g" { source = "git::https://x/y.git" }`},
			want:  `or add a tf_library with module_source = "git::https://x/y.git"`,
		},
		{
			name:  "git ref differs",
			files: map[string]string{"app/main.tf": `module "g" { source = "git::https://x/y.git?ref=v2" }`, "external/y/main.tf": ``},
			deps:  []dep{{Label: "@y", Source: "git::https://x/y.git?ref=v1", Dir: "external/y"}},
			want:  `no dep provides source "git::https://x/y.git?ref=v2"`,
		},
		{
			name:  "unused direct dep",
			files: map[string]string{"app/main.tf": ``, "external/y/main.tf": ``},
			deps:  []dep{{Label: "@y//:y", Source: "git::https://x/y.git", Dir: "external/y", Direct: true}},
			want:  `no module call uses @y//:y (module_source "git::https://x/y.git")`,
		},
		{
			name:  "duplicate source",
			files: map[string]string{"app/main.tf": ``},
			deps: []dep{
				{Label: "@a", Source: "acme/x/aws", Version: "1.0.0", Dir: "external/a"},
				{Label: "@b", Source: "registry.opentofu.org/ACME/x/aws", Version: "1.0.0", Dir: "external/b"},
			},
			want: `@a and @b both have module_source`,
		},
		{
			name:  "registry dep without version",
			files: map[string]string{"app/main.tf": `module "m" { source = "acme/x/aws" }`, "external/a/main.tf": ``},
			deps:  []dep{{Label: "@a", Source: "acme/x/aws", Dir: "external/a"}},
			want:  `must set module_version`,
		},
		{
			name:  "missing subdir",
			files: map[string]string{"app/main.tf": `module "m" { source = "acme/x/aws//nope" }`, "external/a/main.tf": ``},
			deps:  []dep{{Label: "@a", Source: "acme/x/aws", Version: "1.0.0", Dir: "external/a"}},
			want:  `@a (module_source "acme/x/aws") has no directory "nope"`,
		},
		{
			name:  "local source not in deps",
			files: map[string]string{"app/main.tf": `module "m" { source = "../other" }`},
			want:  `is not in the work tree; add the tf_library in that directory to deps`,
		},
		{
			name:  "local source into another repository",
			files: map[string]string{"app/main.tf": `module "m" { source = "../external/a" }`, "external/a/main.tf": ``},
			want:  `is in another Bazel repository`,
		},
		{
			name:  "local source out of a repository",
			files: map[string]string{"app/main.tf": `module "m" { source = "git::https://a" }`, "external/a/main.tf": `module "n" { source = "../../app" }`},
			deps:  []dep{{Label: "@a", Source: "git::https://a", Dir: "external/a"}},
			want:  `is in another Bazel repository`,
		},
		{
			name:  "non-literal source",
			files: map[string]string{"app/main.tf": `module "m" { source = var.where }`},
			want:  "needs `source` to be a literal string",
		},
		{
			name:  "self-call",
			files: map[string]string{"app/main.tf": `module "m" { source = "./" }`},
			want:  "nested more than",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tree := writeTree(t, tc.files)
			_, err := build(tree, "app", "//app", tc.deps)
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

// A dep for a submodule wins over the dep for the module containing it, and
// a call deeper than either module_source uses the rest as a subdirectory.
func TestBuild_LongestMatch(t *testing.T) {
	tree := writeTree(t, map[string]string{
		"app/main.tf":                   `module "m" { source = "acme/n/aws//modules/x/y" }`,
		"external/whole+/main.tf":       ``,
		"external/sub+/x/y/main.tf":     ``,
		"external/whole+/modules/x/y/z": ``,
	})
	deps := []dep{
		{Label: "@whole", Source: "acme/n/aws", Version: "1.0.0", Dir: "external/whole+", Direct: true},
		{Label: "@sub", Source: "acme/n/aws//modules", Version: "1.0.0", Dir: "external/sub+", Direct: true},
	}
	_, err := build(tree, "app", "//app", deps)
	if err == nil || !strings.Contains(err.Error(), "no module call uses @whole") {
		t.Fatalf("expected @whole to go unused, got %v", err)
	}
	got, err := build(tree, "app", "//app", deps[1:])
	if err != nil {
		t.Fatal(err)
	}
	want := []Record{
		{Key: "", Source: "", Dir: "."},
		{Key: "m", Source: "acme/n/aws//modules/x/y", Version: "1.0.0", Dir: "../external/sub+/x/y"},
	}
	if !reflect.DeepEqual(got.Modules, want) {
		t.Errorf("got %+v, want %+v", got.Modules, want)
	}
}
