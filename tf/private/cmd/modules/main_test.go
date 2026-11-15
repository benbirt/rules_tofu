package main

import (
	"errors"
	"fmt"
	"io/fs"
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
  source  = "Acme/Network/aws"
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
	got, err := build(tree, "app", "//app:app", deps, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []Record{
		{Key: "", Source: "", Dir: "."},
		{Key: "git", Source: "git::https://example.com/infra.git//vpc?ref=v1.0.0", Dir: "../external/infra+/vpc"},
		{Key: "git.x", Source: "acme/dns/aws", Version: "0.1.0", Dir: "../external/dns+"},
		{Key: "local", Source: "../shared", Dir: "../shared"},
		{Key: "local.inner", Source: "./inner", Dir: "../shared/inner"},
		{Key: "net", Source: "Acme/Network/aws", Version: "1.2.3", Dir: "../external/net+"},
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
	got, err := build(tree, "", "//:root", nil, nil)
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
	got, err := build(tree, "app", "//app", nil, nil)
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
			name:  "unused direct registry dep",
			files: map[string]string{"app/main.tf": ``, "external/y/main.tf": ``},
			deps:  []dep{{Label: "@y//:y", Source: "acme/net/aws", Version: "1.0.0", Dir: "external/y", Direct: true}},
			want:  `no module call uses @y//:y (module_source "acme/net/aws")`,
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
			name:  "matched source with no configuration in the subdir",
			files: map[string]string{"app/main.tf": `module "m" { source = "acme/x/aws//docs" }`, "external/a/main.tf": ``, "external/a/docs/README.md": ``},
			deps:  []dep{{Label: "@a", Source: "acme/x/aws", Version: "1.0.0", Dir: "external/a"}},
			want:  `@a (module_source "acme/x/aws") has no .tf, .tf.json, .tofu or .tofu.json files in "docs"`,
		},
		{
			name:  "matched source with no configuration",
			files: map[string]string{"app/main.tf": `module "m" { source = "acme/x/aws" }`, "external/a/README.md": ``},
			deps:  []dep{{Label: "@a", Source: "acme/x/aws", Version: "1.0.0", Dir: "external/a"}},
			want:  `@a (module_source "acme/x/aws") has no .tf, .tf.json, .tofu or .tofu.json files`,
		},
		{
			name:  "matched source with no directory",
			files: map[string]string{"app/main.tf": `module "m" { source = "acme/x/aws" }`},
			deps:  []dep{{Label: "@a", Source: "acme/x/aws", Version: "1.0.0", Dir: "external/a"}},
			want:  `@a (module_source "acme/x/aws") has no .tf, .tf.json, .tofu or .tofu.json files`,
		},
		{
			name:  "backslash local source outside the work tree",
			files: map[string]string{"app/main.tf": `module "m" { source = "..\\..\\outside" }`},
			want:  `source "..\\..\\outside" is outside the work tree`,
		},
		{
			name:  "self-call",
			files: map[string]string{"app/main.tf": `module "m" { source = "./" }`},
			want:  `source "./" is the directory of the root module, an ancestor of this call, so the module calls form a cycle`,
		},
		{
			name: "cycle through a called module",
			files: map[string]string{
				"app/main.tf":     `module "b" { source = "./b" }`,
				"app/b/main.tf":   `module "c" { source = "./c" }`,
				"app/b/c/main.tf": `module "back" { source = "../" }`,
			},
			want: `module "back" (app/b/c/main.tf:1): source "../" is the directory of module "b", an ancestor of this call, so the module calls form a cycle`,
		},
		{
			// Two calls into a cycle at each level once branched 2^depth times
			// before any depth limit stopped it (#58); a cycle is reported as
			// soon as a call reaches a module above it.
			name: "cycle reached by two calls",
			files: map[string]string{
				"app/main.tf":   "module \"b1\" { source = \"./b\" }\nmodule \"b2\" { source = \"./b\" }",
				"app/b/main.tf": "module \"a1\" { source = \"../\" }\nmodule \"a2\" { source = \"../\" }",
			},
			want: `module "a1" (app/b/main.tf:1): source "../" is the directory of the root module`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tree := writeTree(t, tc.files)
			_, err := build(tree, "app", "//app", tc.deps, nil)
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
// The submodule's own library provides a call into it, not the module that
// contains it, however either address is written: the parent's here is the
// longer string (#97).
func TestBuild_MostSpecificMatch(t *testing.T) {
	tree := writeTree(t, map[string]string{
		"app/main.tf":                   `module "m" { source = "acme/n/aws//modules/x/y" }`,
		"external/whole+/main.tf":       ``,
		"external/sub+/x/y/main.tf":     ``,
		"external/whole+/modules/x/y/z": ``,
	})
	deps := []dep{
		{Label: "@whole", Source: "registry.opentofu.org/acme/n/aws", Version: "1.0.0", Dir: "external/whole+", Direct: true},
		{Label: "@sub", Source: "acme/n/aws//modules", Version: "1.0.0", Dir: "external/sub+", Direct: true},
	}
	_, err := build(tree, "app", "//app", deps, nil)
	if err == nil || !strings.Contains(err.Error(), "no module call uses @whole") {
		t.Fatalf("expected @whole to go unused, got %v", err)
	}
	got, err := build(tree, "app", "//app", deps[1:], nil)
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

// A library with module_source is in its own listing, never as a direct dep,
// so its files can call its address with `//<subdir>` as its callers do
// (#95). What such a module calls is still the library's own configuration.
func TestBuild_SelfCall(t *testing.T) {
	files := map[string]string{
		"external/net+/main.tf":           `module "y" { source = "acme/net/aws//modules/y" }`,
		"external/net+/modules/y/main.tf": `module "dns" { source = "acme/dns/aws" }`,
		"external/dns+/main.tf":           ``,
	}
	self := dep{Label: "@net", Source: "acme/net/aws", Version: "1.0.0", Dir: "external/net+"}
	dns := dep{Label: "@dns", Source: "acme/dns/aws", Version: "0.1.0", Dir: "external/dns+", Direct: true}

	t.Run("resolved in the library's own files", func(t *testing.T) {
		got, err := build(writeTree(t, files), "external/net+", "@net", []dep{self, dns}, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := []Record{
			{Key: "", Source: "", Dir: "."},
			{Key: "y", Source: "acme/net/aws//modules/y", Version: "1.0.0", Dir: "modules/y"},
			{Key: "y.dns", Source: "acme/dns/aws", Version: "0.1.0", Dir: "../dns+"},
		}
		if !reflect.DeepEqual(got.Modules, want) {
			t.Errorf("got\n%+v\nwant\n%+v", got.Modules, want)
		}
	})

	t.Run("a dep nothing calls is still unused", func(t *testing.T) {
		unused := dep{Label: "@unused", Source: "acme/unused/aws", Version: "1.0.0", Dir: "external/unused+", Direct: true}
		_, err := build(writeTree(t, files), "external/net+", "@net", []dep{self, dns, unused}, nil)
		if err == nil || !strings.Contains(err.Error(), "no module call uses @unused") || strings.Contains(err.Error(), "@net (") {
			t.Errorf("got %v", err)
		}
	})

	t.Run("a dep declaring the submodule wins", func(t *testing.T) {
		withSub := map[string]string{"external/y+/main.tf": ``}
		for name, content := range files {
			withSub[name] = content
		}
		y := dep{Label: "@y", Source: "acme/net/aws//modules/y", Version: "1.0.0", Dir: "external/y+", Direct: true}
		got, err := build(writeTree(t, withSub), "external/net+", "@net", []dep{self, y, dns}, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := []Record{
			{Key: "", Source: "", Dir: "."},
			{Key: "y", Source: "acme/net/aws//modules/y", Version: "1.0.0", Dir: "../y+"},
		}
		if !reflect.DeepEqual(got.Modules, want) {
			t.Errorf("got\n%+v\nwant\n%+v", got.Modules, want)
		}
	})

	t.Run("its own address is a cycle", func(t *testing.T) {
		tree := writeTree(t, map[string]string{"external/net+/main.tf": `module "me" { source = "acme/net/aws" }`})
		_, err := build(tree, "external/net+", "@net", []dep{self}, nil)
		if err == nil || !strings.Contains(err.Error(), "so the module calls form a cycle") {
			t.Errorf("got %v", err)
		}
	})

	t.Run("a caller's deps are not used from inside it", func(t *testing.T) {
		withApp := map[string]string{"app/main.tf": `module "net" { source = "acme/net/aws" }`}
		for name, content := range files {
			withApp[name] = content
		}
		direct := self
		direct.Direct = true
		_, err := build(writeTree(t, withApp), "app", "//app", []dep{direct, dns}, nil)
		if err == nil || !strings.Contains(err.Error(), "no module call uses @dns") {
			t.Errorf("got %v", err)
		}
	})
}

// A direct dep counts as used when its dir is root, or when one of the
// target's files calls it by module_source or by a relative path to one of
// its directories, from above or below (#57).
func TestBuild_DirectUse(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		root  string
		deps  []dep
	}{
		{
			name: "same package",
			files: map[string]string{
				"vpc/main.tf":             `module "sub" { source = "./modules/sub" }`,
				"vpc/modules/sub/main.tf": `module "dns" { source = "acme/dns/aws" }`,
				"external/dns+/main.tf":   ``,
			},
			root: "vpc",
			deps: []dep{
				{Label: "//vpc:vpc_lib", Source: "github.com/acme/infra//vpc", Dir: "vpc", Direct: true},
				{Label: "@dns//:dns", Source: "acme/dns/aws", Version: "1.0.0", Dir: "external/dns+", Direct: true},
			},
		},
		{
			name: "local call to a sibling",
			files: map[string]string{
				"app/main.tf":           `module "net" { source = "../network" }`,
				"network/main.tf":       `module "inner" { source = "./inner" }`,
				"network/inner/main.tf": ``,
			},
			root: "app",
			deps: []dep{{Label: "//network", Source: "github.com/acme/infra//network", Dir: "network", Direct: true}},
		},
		{
			name: "local call up out of the root",
			files: map[string]string{
				"net/app/main.tf":    `module "s" { source = "../subnet" }`,
				"net/subnet/main.tf": ``,
			},
			root: "net/app",
			deps: []dep{{Label: "//net", Source: "github.com/acme/net", Dir: "net", Dirs: []string{"net/subnet"}, Direct: true}},
		},
		{
			name: "example calling the repository's root package",
			files: map[string]string{
				"examples/complete/main.tf": `module "m" { source = "../../" }`,
				"main.tf":                   ``,
			},
			root: "examples/complete",
			deps: []dep{{Label: "//:lib", Source: "acme/lib/aws", Version: "1.0.0", Dir: "", Direct: true}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tree := writeTree(t, tc.files)
			if _, err := build(tree, tc.root, "//"+tc.root, tc.deps, nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A direct dep called only from another library's files is unused (#57). This
// holds whether that library is reached by module_source or relative path,
// sets module_source or not, or sits in a subdirectory of root.
func TestBuild_TransitiveCallDoesNotUseDirectDep(t *testing.T) {
	for _, tc := range []struct {
		name   string
		call   string
		source string
	}{
		{name: "module_source", call: `module "a" { source = "github.com/acme/mod_a" }`, source: "github.com/acme/mod_a"},
		{name: "local", call: `module "a" { source = "../mod_a" }`, source: "github.com/acme/mod_a"},
		{name: "local without module_source", call: `module "a" { source = "../mod_a" }`},
		{name: "subpackage", call: `module "a" { source = "./mod_a" }`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := "mod_a"
			if tc.name == "subpackage" {
				dir = "app/mod_a"
			}
			tree := writeTree(t, map[string]string{
				"app/main.tf":             tc.call,
				dir + "/main.tf":          `module "b" { source = "acme/b/aws" }`,
				"external/mod_b+/main.tf": ``,
			})
			deps := []dep{{Label: "@tf_modules//mod_b", Source: "acme/b/aws", Version: "1.0.0", Dir: "external/mod_b+", Direct: true, Callable: true}}
			if tc.source != "" {
				deps = append(deps, dep{Label: "//mod_a", Source: tc.source, Dir: dir, Dirs: []string{dir}, Direct: true, Callable: true})
			}
			mine := own{srcs: []string{"app/main.tf"}, depDirs: []string{dir}}
			_, err := buildManifest(tree, "app", "//app", deps, nil, "", mine)
			if err == nil || !strings.Contains(err.Error(), "no module call uses @tf_modules//mod_b") {
				t.Fatalf("expected @tf_modules//mod_b to go unused, got %v", err)
			}
			deps[0].Direct = false
			if _, err := buildManifest(tree, "app", "//app", deps, nil, "", mine); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Two calls to one module are not a cycle: each is recorded, and the module
// is walked once per call.
func TestBuild_RepeatedCall(t *testing.T) {
	tree := writeTree(t, map[string]string{
		"app/main.tf":              "module \"x\" { source = \"./shared\" }\nmodule \"y\" { source = \"./shared\" }",
		"app/shared/main.tf":       `module "inner" { source = "./inner" }`,
		"app/shared/inner/main.tf": ``,
	})
	got, err := build(tree, "app", "//app", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []Record{
		{Key: "", Source: "", Dir: "."},
		{Key: "x", Source: "./shared", Dir: "shared"},
		{Key: "x.inner", Source: "./inner", Dir: "shared/inner"},
		{Key: "y", Source: "./shared", Dir: "shared"},
		{Key: "y.inner", Source: "./inner", Dir: "shared/inner"},
	}
	if !reflect.DeepEqual(got.Modules, want) {
		t.Errorf("got\n%+v\nwant\n%+v", got.Modules, want)
	}
}

// A local source written with backslashes, which tofu accepts, names the
// same directory as one with slashes; the manifest keeps the source as
// written and gives the directory with slashes (#58).
func TestBuild_BackslashLocalSource(t *testing.T) {
	tree := writeTree(t, map[string]string{
		"app/main.tf":          `module "m" { source = "..\\shared" }`,
		"shared/main.tf":       `module "inner" { source = ".\\inner\\" }`,
		"shared/inner/main.tf": ``,
	})
	got, err := build(tree, "app", "//app", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []Record{
		{Key: "", Source: "", Dir: "."},
		{Key: "m", Source: `..\shared`, Dir: "../shared"},
		{Key: "m.inner", Source: `.\inner\`, Dir: "../shared/inner"},
	}
	if !reflect.DeepEqual(got.Modules, want) {
		t.Errorf("got\n%+v\nwant\n%+v", got.Modules, want)
	}
}

// A call that lands on the work tree root resolves to "", the root's
// directory everywhere else, so a cycle through the root package is caught
// on the first call back into it.
func TestBuild_RootPackageCycle(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  string
	}{
		{
			name:  "self-call",
			files: map[string]string{"main.tf": `module "m" { source = "./" }`},
			want:  `module "m" (main.tf:1): source "./" is the directory of the root module`,
		},
		{
			name: "call back up",
			files: map[string]string{
				"main.tf":   `module "a" { source = "./a" }`,
				"a/main.tf": `module "up" { source = "../" }`,
			},
			want: `module "up" (a/main.tf:1): source "../" is the directory of the root module`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tree := writeTree(t, tc.files)
			_, err := build(tree, "", "//:root", nil, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

// Every module walked must have its required providers in the plugin tree,
// which --provider lists: the root, a local module, a dep called by
// module_source, and above all a submodule reached by `//<subdir>` outside
// `modules/`, whose providers the dep's generated library can't carry (#76).
// The error names the module, the provider and the label to add; a built-in
// provider needs no plugin.
func TestBuild_RequiredProviders(t *testing.T) {
	files := map[string]string{
		"app/main.tf": `
terraform {
  required_providers {
    random    = { source = "hashicorp/random" }
    terraform = { source = "terraform.io/builtin/terraform" }
  }
}
module "net" { source = "git::https://example.com/network.git//vpc?ref=v1.0.0" }
module "peering" { source = "git::https://example.com/network.git//vpc/peering?ref=v1.0.0" }
module "again" { source = "git::https://example.com/network.git//vpc/peering?ref=v1.0.0" }
module "local" { source = "../shared" }
`,
		"shared/main.tf":            "terraform {\n  required_providers {\n    local = \"~> 2.0\"\n  }\n}",
		"external/net+/vpc/main.tf": "terraform {\n  required_providers {\n    null = { source = \"hashicorp/null\" }\n  }\n}",
		"external/net+/vpc/peering/main.tf": `
terraform {
  required_providers {
    time = { source = "HashiCorp/time" }
  }
}
`,
	}
	deps := []dep{{Label: "@tf_modules//network", Source: "git::https://example.com/network.git//vpc?ref=v1.0.0", Dir: "external/net+/vpc", Direct: true}}
	all := []string{
		"registry.opentofu.org/hashicorp/random",
		"registry.opentofu.org/hashicorp/local",
		"registry.opentofu.org/hashicorp/null",
		"registry.opentofu.org/hashicorp/time",
	}
	tree := writeTree(t, files)
	if _, err := build(tree, "app", "//app:app", deps, all); err != nil {
		t.Fatalf("every provider supplied: %v", err)
	}

	_, err := build(tree, "app", "//app:app", deps, all[:1])
	if err == nil {
		t.Fatal("expected the missing providers to be reported")
	}
	want := `//app:app: nothing in its providers or deps supplies these providers, so ` + "`tofu init`" + ` would not find them:
    module "net" (@tf_modules//network) requires hashicorp/null
    module "peering" (@tf_modules//network, //vpc/peering) requires hashicorp/time
    module "local" requires hashicorp/local
Add "@tf_providers//hashicorp/null", "@tf_providers//hashicorp/time", "@tf_providers//hashicorp/local" to the providers of //app:app.`
	if err.Error() != want {
		t.Errorf("got\n%s\nwant\n%s", err, want)
	}

	_, err = build(tree, "app", "//app:app", deps, all[1:])
	if err == nil || !strings.Contains(err.Error(), "    the root module requires hashicorp/random\n") ||
		!strings.Contains(err.Error(), `Add "@tf_providers//hashicorp/random" to the providers of //app:app.`) {
		t.Errorf("the root module's own requirement is not reported: %v", err)
	}
}

// A provider from another registry can't be vendored, so there is no label
// to add; the error says so instead.
func TestBuild_RequiredProviderFromAnotherRegistry(t *testing.T) {
	tree := writeTree(t, map[string]string{
		"app/main.tf": "terraform {\n  required_providers {\n    aws = { source = \"registry.terraform.io/hashicorp/aws\" }\n  }\n}",
	})
	_, err := build(tree, "app", "//app", nil, nil)
	want := "    the root module requires registry.terraform.io/hashicorp/aws\nrules_tofu can't vendor registry.terraform.io/hashicorp/aws: it only vendors providers from registry.opentofu.org."
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("got %v, want an error containing %q", err, want)
	}
}

// A tf_test's test files: each run block's module is recorded under tofu's
// key and walked like a call from the root module. A local source resolves
// against the root module's directory, not the test file's; a .tofutest.hcl
// shadows the .tftest.hcl of its name; a direct dep only a run uses is used.
func TestBuildManifest_TestRuns(t *testing.T) {
	tree := writeTree(t, map[string]string{
		"app/main.tf":                   `module "s" { source = "./sub" }`,
		"app/sub/main.tf":               ``,
		"app/helper/main.tf":            `module "kid" { source = "./child" }`,
		"app/helper/child/main.tf":      ``,
		"app/root.tftest.hcl":           "run \"rootsetup\" {\n  module {\n    source = \"./helper\"\n  }\n}\n",
		"app/tests/helper.tftest.hcl":   "run \"setup\" {\n  module {\n    source = \"./helper\"\n  }\n}\nrun \"remote\" {\n  module {\n    source = \"git::https://example.com/infra.git//vpc?ref=v1.0.0\"\n  }\n}\nrun \"main\" {\n  command = plan\n}\n",
		"app/tests/shadow.tftest.hcl":   "run \"tf\" {\n  module {\n    source = \"./missing\"\n  }\n}\n",
		"app/tests/shadow.tofutest.hcl": "run \"tofu\" {\n  module {\n    source = \"./sub\"\n  }\n}\n",
		"external/infra+/vpc/main.tf":   ``,
	})
	deps := []dep{
		{Label: "@infra//vpc", Source: "git::https://example.com/infra.git//vpc?ref=v1.0.0", Dir: "external/infra+/vpc", Direct: true},
	}
	got, err := buildLenient(tree, "app", "//app:app_test", deps, nil, "tests")
	if err != nil {
		t.Fatal(err)
	}
	want := []Record{
		{Key: "", Source: "", Dir: "."},
		{Key: "s", Source: "./sub", Dir: "sub"},
		{Key: "test.root.rootsetup", Source: "./helper", Dir: "helper"},
		{Key: "test.root.rootsetup.kid", Source: "./child", Dir: "helper/child"},
		{Key: "test.tests.helper.remote", Source: "git::https://example.com/infra.git//vpc?ref=v1.0.0", Dir: "../external/infra+/vpc"},
		{Key: "test.tests.helper.setup", Source: "./helper", Dir: "helper"},
		{Key: "test.tests.helper.setup.kid", Source: "./child", Dir: "helper/child"},
		{Key: "test.tests.shadow.tofutest.hcl.tofu", Source: "./sub", Dir: "sub"},
	}
	if !reflect.DeepEqual(got.Modules, want) {
		t.Errorf("got\n%+v\nwant\n%+v", got.Modules, want)
	}

	// Without --test-dir the test files are not read, so the dep is unused.
	if _, err := build(tree, "app", "//app:app", deps, nil); err == nil || !strings.Contains(err.Error(), "no module call uses @infra//vpc") {
		t.Errorf("build without tests: err = %v, want the unused dep", err)
	}
}

// A root module with no file at or below its directory has no directory in
// the work tree, and one with no files at all has no work tree: an empty
// module either way, as it is when the directory is there (issue #119).
func TestBuild_EmptyRoot(t *testing.T) {
	only := []Record{{Key: "", Source: "", Dir: "."}}
	tree := writeTree(t, map[string]string{
		"lib/main.tf":           ``,
		"present/sub/main.tf":   ``,
		"external/net+/main.tf": ``,
	})
	for name, tc := range map[string]struct{ workTree, root string }{
		"no directory":            {tree, "absent"},
		"no directory, nested":    {tree, "absent/deeper"},
		"no work tree":            {filepath.Join(tree, "nowhere"), "pkg"},
		"no work tree, root pkg":  {filepath.Join(tree, "nowhere"), ""},
		"a directory, no config":  {tree, "present"},
		"an external, empty root": {tree, "external/empty+"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := build(tc.workTree, tc.root, "//x:x", nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Modules, only) {
				t.Errorf("got %+v, want only the root", got.Modules)
			}
		})
	}

	// An empty root calls nothing, so a dep that must be called is unused.
	deps := []dep{{Label: "@net//:net", Source: "acme/network/aws", Version: "1.2.3", Dir: "external/net+", Direct: true}}
	if _, err := build(tree, "absent", "//absent:lib", deps, nil); err == nil || !strings.Contains(err.Error(), "no module call uses @net//:net") {
		t.Errorf("err = %v, want the unused dep", err)
	}

	// Something that isn't a directory where the root should be is not an
	// empty module.
	if _, err := build(tree, "lib/main.tf", "//x:x", nil, nil); err == nil {
		t.Error("a file in place of the root module's directory was taken for an empty module")
	}
	// Nor is a path through a file, which fails differently: there the
	// stat itself does, and not because nothing is there.
	if _, err := build(tree, "lib/main.tf/sub", "//x:x", nil, nil); err == nil {
		t.Error("a root module's directory below a file was taken for an empty module")
	}
}

// A tf_test of an empty root: its directory is there, since the test files
// are at or below it, and holds no configuration. The runs' modules are
// still recorded.
func TestBuildManifest_EmptyRootTestRuns(t *testing.T) {
	tree := writeTree(t, map[string]string{
		"lib/main.tf":                 ``,
		"empty/tests/main.tftest.hcl": "run \"lib\" {\n  module {\n    source = \"../lib\"\n  }\n}\n",
	})
	got, err := buildLenient(tree, "empty", "//empty:test", nil, nil, "tests")
	if err != nil {
		t.Fatal(err)
	}
	want := []Record{
		{Key: "", Source: "", Dir: "."},
		{Key: "test.tests.main.lib", Source: "../lib", Dir: "../lib"},
	}
	if !reflect.DeepEqual(got.Modules, want) {
		t.Errorf("got\n%+v\nwant\n%+v", got.Modules, want)
	}
}

// Test files in the root module's directory only: "." reads no other.
func TestBuildManifest_TestRunsRootOnly(t *testing.T) {
	tree := writeTree(t, map[string]string{
		"main.tf":              ``,
		"helper/main.tf":       ``,
		"x.tftest.hcl":         "run \"a\" {\n  module {\n    source = \"./helper\"\n  }\n}\n",
		"tests/y.tftest.hcl":   "run \"b\" {\n  module {\n    source = \"./nowhere\"\n  }\n}\n",
		"tests/helper/main.tf": ``,
	})
	got, err := buildLenient(tree, "", "//:root_test", nil, nil, ".")
	if err != nil {
		t.Fatal(err)
	}
	want := []Record{
		{Key: "", Source: "", Dir: "."},
		{Key: "test.x.a", Source: "./helper", Dir: "helper"},
	}
	if !reflect.DeepEqual(got.Modules, want) {
		t.Errorf("got\n%+v\nwant\n%+v", got.Modules, want)
	}
}

func TestBuildManifest_TestRunErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		files map[string]string
		deps  []dep
		want  string
	}{
		"helper_missing": {
			files: map[string]string{
				"app/main.tf":            ``,
				"app/tests/t.tftest.hcl": "run \"setup\" {\n  module {\n    source = \"./helper\"\n  }\n}\n",
			},
			want: `run "setup" (app/tests/t.tftest.hcl:3): source "./helper" is not in the work tree`,
		},
		"source_unprovided": {
			files: map[string]string{
				"app/main.tf":            ``,
				"app/tests/t.tftest.hcl": "run \"setup\" {\n  module {\n    source = \"acme/net/aws\"\n  }\n}\n",
			},
			want: `no dep provides source "acme/net/aws"`,
		},
		"helper_requires_provider": {
			files: map[string]string{
				"app/main.tf":            ``,
				"app/helper/main.tf":     "terraform {\n  required_providers {\n    time = { source = \"hashicorp/time\" }\n  }\n}\n",
				"app/tests/t.tftest.hcl": "run \"setup\" {\n  module {\n    source = \"./helper\"\n  }\n}\n",
			},
			want: `module "test.tests.t.setup" requires hashicorp/time`,
		},
		"cycle_back_to_root": {
			files: map[string]string{
				"app/main.tf":            ``,
				"app/helper/main.tf":     `module "back" { source = "../" }`,
				"app/tests/t.tftest.hcl": "run \"setup\" {\n  module {\n    source = \"./helper\"\n  }\n}\n",
			},
			want: "form a cycle",
		},
	} {
		t.Run(name, func(t *testing.T) {
			tree := writeTree(t, tc.files)
			_, err := buildLenient(tree, "app", "//app:app_test", tc.deps, nil, "tests")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

// build is buildLenient for a rule with no test files.
func build(workTree, root, label string, deps []dep, provided []string) (*Manifest, error) {
	return buildLenient(workTree, root, label, deps, provided, "")
}

// buildLenient calls buildManifest with inputs that satisfy strict deps. It
// puts every file at or below root in srcs, marks every dep callable, adds
// every work-tree directory to depDirs, and lists every provider in
// own.providers.
func buildLenient(workTree, root, label string, deps []dep, provided []string, testDir string) (*Manifest, error) {
	callable := make([]dep, len(deps))
	for i, d := range deps {
		d.Callable = true
		if d.Dirs == nil {
			d.Dirs = []string{d.Dir}
		}
		callable[i] = d
	}
	mine := own{providers: provided}
	err := filepath.WalkDir(workTree, func(p string, e os.DirEntry, err error) error {
		// A rule with no files at all has no work tree.
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(workTree, p)
		if err != nil {
			return err
		}
		if rel = filepath.ToSlash(rel); rel == "." {
			rel = ""
		}
		switch {
		case e.IsDir():
			mine.depDirs = append(mine.depDirs, rel)
		case within(dirOf(rel), root):
			mine.srcs = append(mine.srcs, rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return buildManifest(workTree, root, label, callable, provided, testDir, mine)
}

// Strict deps (#135) checks own.srcs, walked own.data, and run blocks in a
// test's own.srcs against own.providers, callable deps and depDirs. Files from
// other targets are not checked here, even when they sit in root.
func TestBuildManifest_StrictDeps(t *testing.T) {
	const (
		random = "registry.opentofu.org/hashicorp/random"
		null   = "registry.opentofu.org/hashicorp/null"
	)
	requires := func(name string) string {
		return fmt.Sprintf("terraform {\n  required_providers {\n    %s = { source = \"hashicorp/%s\" }\n  }\n}\n", name, name)
	}
	net := dep{Label: "@net", Source: "acme/net/aws", Version: "1.0.0", Dir: "external/net+", Direct: true, Callable: true}
	dns := dep{Label: "@dns", Source: "acme/dns/aws", Version: "1.0.0", Dir: "external/dns+"}
	for _, tc := range []struct {
		name     string
		files    map[string]string
		deps     []dep
		provided []string
		own      own
		testDir  string
		root     string
		want     string
	}{
		{
			name:     "a provider only deps list",
			files:    map[string]string{"app/main.tf": requires("random") + `module "lib" { source = "../lib" }`, "lib/main.tf": requires("random") + requires("null")},
			provided: []string{random, null},
			own:      own{srcs: []string{"app/main.tf"}, depDirs: []string{"lib"}},
			want: `//app: its files require these providers, which are not in its providers:
    app/main.tf:3 requires hashicorp/random
Add "@tf_providers//hashicorp/random" to the providers of //app.`,
		},
		{
			name:     "a provider listed directly",
			files:    map[string]string{"app/main.tf": requires("random") + `module "lib" { source = "../lib" }`, "lib/main.tf": requires("null")},
			provided: []string{null},
			own:      own{srcs: []string{"app/main.tf"}, depDirs: []string{"lib"}, providers: []string{random}},
		},
		{
			name:     "a dep's file in the root module's directory is the dep's",
			files:    map[string]string{"app/backend.tf": ``, "app/main.tf": requires("null") + `module "dns" { source = "acme/dns/aws" }`, "external/dns+/main.tf": ``},
			deps:     []dep{dns},
			provided: []string{null},
			own:      own{srcs: []string{"app/backend.tf"}, depDirs: []string{"app"}},
		},
		{
			name:     "a file in srcs is checked wherever it is",
			files:    map[string]string{"app/main.tf": ``, "app/modules/unused/main.tf": requires("null")},
			provided: []string{null},
			own:      own{srcs: []string{"app/main.tf", "app/modules/unused/main.tf"}},
			want:     "    app/modules/unused/main.tf:3 requires hashicorp/null\n",
		},
		{
			name:     "absent and indirect providers are told apart",
			files:    map[string]string{"app/main.tf": requires("random") + requires("null")},
			provided: []string{null},
			own:      own{srcs: []string{"app/main.tf"}},
			want: "//app: nothing in its providers or deps supplies these providers, so `tofu init` would not find them:\n" +
				"    the root module requires hashicorp/random\n" +
				"//app: its files require these providers, which are not in its providers:\n" +
				"    app/main.tf:8 requires hashicorp/null\n" +
				`Add "@tf_providers//hashicorp/random", "@tf_providers//hashicorp/null" to the providers of //app.`,
		},
		{
			name:  "a module_source call to a dep's dep",
			files: map[string]string{"app/main.tf": `module "net" { source = "acme/net/aws" }` + "\n" + `module "dns" { source = "acme/dns/aws" }`, "external/net+/main.tf": `module "dns" { source = "acme/dns/aws" }`, "external/dns+/main.tf": ``},
			deps:  []dep{net, dns},
			own:   own{srcs: []string{"app/main.tf"}},
			want:  `//app: module "dns" (app/main.tf:2): source "acme/dns/aws" resolves to "@dns", which is not in the deps of //app; add "@dns" to the deps of //app`,
		},
		{
			name:  "a dep calls its own dep",
			files: map[string]string{"app/main.tf": `module "net" { source = "acme/net/aws" }`, "external/net+/main.tf": `module "dns" { source = "acme/dns/aws" }`, "external/dns+/main.tf": ``},
			deps:  []dep{net, dns},
			own:   own{srcs: []string{"app/main.tf"}},
		},
		{
			name:  "a relative call to a dep's dep's directory",
			files: map[string]string{"app/main.tf": `module "a" { source = "../a" }` + "\n" + `module "b" { source = "../b" }`, "a/main.tf": `module "b" { source = "../b" }`, "b/main.tf": ``},
			own:   own{srcs: []string{"app/main.tf"}, depDirs: []string{"a"}},
			want:  `//app: module "b" (app/main.tf:2): source "../b" resolves to the directory "b", which has no files from //app or from a tf_library in its deps; add the tf_library whose files are in "b" to the deps of //app`,
		},
		{
			name:  "a dep calls its own dep's directory",
			files: map[string]string{"app/main.tf": `module "a" { source = "../a" }`, "a/main.tf": `module "b" { source = "../b" }`, "b/main.tf": ``},
			own:   own{srcs: []string{"app/main.tf"}, depDirs: []string{"a"}},
		},
		{
			name:  "another library's directory in a direct dep's package",
			files: map[string]string{"app/main.tf": `module "p" { source = "../net/peering" }`, "net/vpc/main.tf": ``, "net/peering/main.tf": ``},
			own:   own{srcs: []string{"app/main.tf"}, depDirs: []string{"net/vpc"}},
			want:  `source "../net/peering" resolves to the directory "net/peering", which has no files from //app or from a tf_library in its deps`,
		},
		{
			name:  "a directory of the rule's own data",
			files: map[string]string{"app/main.tf": `module "t" { source = "../templates" }`, "templates/motd.tftpl": ``},
			own:   own{srcs: []string{"app/main.tf"}, data: []string{"templates/motd.tftpl"}},
		},
		{
			name:  "a directory of a dep's dep's data",
			files: map[string]string{"app/main.tf": `module "t" { source = "../templates" }`, "templates/main.tf": ``},
			own:   own{srcs: []string{"app/main.tf"}},
			want:  `source "../templates" resolves to the directory "templates", which has no files from //app or from a tf_library in its deps`,
		},
		{
			name:    "a run block's module is the test's own call",
			files:   map[string]string{"app/main.tf": ``, "app/tests/x.tftest.hcl": "run \"r\" {\n  module {\n    source = \"acme/dns/aws\"\n  }\n}", "external/dns+/main.tf": ``},
			deps:    []dep{dns},
			own:     own{srcs: []string{"app/tests/x.tftest.hcl"}, depDirs: []string{"app"}},
			testDir: "tests",
			want:    `source "acme/dns/aws" resolves to "@dns", which is not in the deps of //app`,
		},
		{
			name:    "a test file of the module under test is the module's",
			files:   map[string]string{"app/main.tf": ``, "app/tests/x.tftest.hcl": "run \"r\" {\n  module {\n    source = \"acme/dns/aws\"\n  }\n}", "external/dns+/main.tf": ``},
			deps:    []dep{dns},
			own:     own{srcs: []string{"app/tests/mine.tftest.hcl"}, depDirs: []string{"app"}},
			testDir: "tests",
		},
		{
			name:  "an override in the rule's own file makes the call its own",
			files: map[string]string{"app/main.tf": `module "m" { source = "acme/net/aws" }`, "app/m_override.tf": `module "m" { source = "acme/dns/aws" }`, "external/net+/main.tf": ``, "external/dns+/main.tf": ``},
			deps:  []dep{{Label: "@net", Source: "acme/net/aws", Version: "1.0.0", Dir: "external/net+"}, dns},
			own:   own{srcs: []string{"app/m_override.tf"}, depDirs: []string{"app"}},
			want:  `module "m" (app/m_override.tf:1): source "acme/dns/aws" resolves to "@dns", which is not in the deps of //app`,
		},
		{
			name:  "a generated library is named by its hub label",
			files: map[string]string{"app/main.tf": `module "dns" { source = "acme/dns/aws" }`, "external/dns+/main.tf": ``},
			deps:  []dep{{Label: "@@rules_tofu++tf_modules+tf_modules_dns//:_rules_tofu_module", Source: "acme/dns/aws", Version: "1.0.0", Dir: "external/dns+"}},
			own:   own{srcs: []string{"app/main.tf"}},
			want:  `resolves to "@tf_modules//acme/dns/aws", which is not in the deps of //app; add "@tf_modules//acme/dns/aws" to the deps of //app`,
		},
		{
			name:  "a generated git library is named by its hub label",
			files: map[string]string{"app/main.tf": `module "net" { source = "git::https://example.com/net.git?ref=v1" }`, "external/net+/main.tf": ``},
			deps:  []dep{{Label: "@@rules_tofu++tf_modules+tf_modules_my-net.v1//:_rules_tofu_module", Source: "git::https://example.com/net.git?ref=v1", Dir: "external/net+"}},
			own:   own{srcs: []string{"app/main.tf"}},
			want:  `add "@tf_modules//my-net.v1" to the deps of //app`,
		},
		{
			name:  "a call into a direct dep's package uses only the dep whose files are there",
			files: map[string]string{"net/app/main.tf": `module "s" { source = "./sub" }`, "net/app/sub/main.tf": ``, "net/main.tf": ``},
			deps:  []dep{{Label: "//net", Source: "github.com/acme/net", Dir: "net", Dirs: []string{"net"}, Direct: true, Callable: true}},
			own:   own{srcs: []string{"net/app/main.tf", "net/app/sub/main.tf"}},
			root:  "net/app",
			want:  `no module call uses //net`,
		},
		{
			name:    "the root module's calls are not the test's",
			files:   map[string]string{"app/main.tf": `module "dns" { source = "acme/dns/aws" }`, "app/tests/x.tftest.hcl": ``, "external/dns+/main.tf": ``},
			deps:    []dep{dns},
			own:     own{srcs: []string{"app/tests/x.tftest.hcl"}, depDirs: []string{"app"}},
			testDir: "tests",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := tc.root
			if root == "" {
				root = "app"
			}
			_, err := buildManifest(writeTree(t, tc.files), root, "//app", tc.deps, tc.provided, tc.testDir, tc.own)
			switch {
			case tc.want == "" && err != nil:
				t.Fatal(err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("got\n%v\nwant an error containing\n%s", err, tc.want)
			}
		})
	}
}
