package tfconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestTestFiles_TofuShadowsTf(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"a.tftest.hcl", "a.tofutest.hcl", "b.tftest.hcl", "c.tofutest.json", "c.tftest.json",
		"d.tftest.json", "main.tf", "notes.hcl", "x.tftest.hcl.bak",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "e.tftest.hcl"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := TestFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range got {
		names = append(names, filepath.Base(f))
	}
	want := []string{"a.tofutest.hcl", "b.tftest.hcl", "c.tofutest.json", "d.tftest.json"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("TestFiles = %v, want %v", names, want)
	}
	if files, err := TestFiles(filepath.Join(dir, "missing")); err != nil || files != nil {
		t.Errorf("TestFiles(missing) = %v, %v; want nil, nil", files, err)
	}
}

func TestTestRunKey(t *testing.T) {
	for _, tc := range []struct{ file, run, want string }{
		{"root.tftest.hcl", "setup", "test.root.setup"},
		{"tests/helper.tftest.hcl", "setup", "test.tests.helper.setup"},
		// tofu drops only `.tftest.hcl`; the other extensions stay in the key.
		{"tests/helper.tofutest.hcl", "setup", "test.tests.helper.tofutest.hcl.setup"},
		{"a/b/x.tftest.json", "deep", "test.a.b.x.tftest.json.deep"},
		{"a/b/x.tofutest.json", "deep", "test.a.b.x.tofutest.json.deep"},
	} {
		if got := TestRunKey(filepath.FromSlash(tc.file), tc.run); got != tc.want {
			t.Errorf("TestRunKey(%q, %q) = %q, want %q", tc.file, tc.run, got, tc.want)
		}
	}
}

func TestReadTestCalls(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"helper.tftest.hcl": `
run "setup" {
  module {
    source = "./helper"
  }
}

run "remote" {
  module {
    source  = "acme/network/aws//modules/vpc"
    version = "~> 1.0"
  }
  variables {
    name = run.setup.name
  }
}

run "main" {
  command = plan
  assert {
    condition     = var.name != ""
    error_message = "x"
  }
}
`,
		"shadowed.tftest.hcl":   `run "tf" { module { source = "./wrong" } }`,
		"shadowed.tofutest.hcl": "run \"tofu\" {\n  module {\n    source = \"./right\"\n  }\n}\n",
		"json.tftest.json":      `{"run": {"j": {"module": {"source": "./json"}}}}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	files, err := TestFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	// The shadowed .tftest.hcl is listed too: ReadTestCalls drops it itself.
	files = append(files, filepath.Join(dir, "shadowed.tftest.hcl"))
	calls, err := ReadTestCalls(files)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range calls {
		got = append(got, filepath.Base(c.Range.Filename)+" "+c.Name+" "+c.Source)
	}
	want := []string{
		"helper.tftest.hcl setup ./helper",
		"helper.tftest.hcl remote acme/network/aws//modules/vpc",
		"json.tftest.json j ./json",
		"shadowed.tofutest.hcl tofu ./right",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ReadTestCalls = %q, want %q", got, want)
	}
}

func TestReadTestCalls_Errors(t *testing.T) {
	for name, tc := range map[string]struct{ content, want string }{
		"duplicate_run": {"run \"a\" {}\nrun \"a\" {\n  module {\n    source = \"./x\"\n  }\n}\n", `duplicate run "a"`},
		"no_source":     {"run \"a\" {\n  module {}\n}\n", "has no source"},
		"non_literal":   {"run \"a\" {\n  module {\n    source = var.x\n  }\n}\n", "literal string"},
	} {
		t.Run(name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "t.tftest.hcl")
			if err := os.WriteFile(file, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := ReadTestCalls([]string{file})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("ReadTestCalls error = %v, want containing %q", err, tc.want)
			}
		})
	}
}
