package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func decode(t *testing.T, src string) []map[string]any {
	t.Helper()
	var out strings.Builder
	if err := convert([]byte(src), "test.hcl", &out); err != nil {
		t.Fatalf("convert: %v", err)
	}
	var doc struct {
		Providers []map[string]any `json:"providers"`
	}
	if err := json.Unmarshal([]byte(out.String()), &doc); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	return doc.Providers
}

func TestConvert_TofuLock(t *testing.T) {
	got := decode(t, `# This file is maintained automatically by "tofu init".
# Manual edits may be lost in future updates.

provider "registry.opentofu.org/hashicorp/null" {
  version     = "3.2.4"
  constraints = ">= 3.0.0, 3.2.4"
  hashes = [
    "h1:346niW/SBt/jtCZctHefjkQGqtX9YgxSkCAKeCzQhXw=",
    "zh:3d106c7e32a929e2843f732625a582e562ff09120021e510a51a6f5d01175b8d",
  ]
}

provider "registry.opentofu.org/posthog/posthog" {
  version = "1.0.24"
  hashes  = ["zh:1769783386610bed8bb1e861a119fe25058be41895e3996d9216dd6bb8a7aee3"]
}
`)
	if len(got) != 2 {
		t.Fatalf("got %d providers, want 2: %v", len(got), got)
	}
	if got[0]["address"] != "registry.opentofu.org/hashicorp/null" || got[0]["version"] != "3.2.4" {
		t.Errorf("first provider = %v", got[0])
	}
	if hashes := got[0]["hashes"].([]any); len(hashes) != 2 || hashes[1] != "zh:3d106c7e32a929e2843f732625a582e562ff09120021e510a51a6f5d01175b8d" {
		t.Errorf("hashes = %v", hashes)
	}
	if _, ok := got[0]["constraints"]; ok {
		t.Errorf("constraints should not be emitted: %v", got[0])
	}
}

func TestConvert_ToleratesUnknownAndMissing(t *testing.T) {
	got := decode(t, `
// Written by some other tool.
/* A block
   comment. */
provider "registry.opentofu.org/a/b" {
  future = "ignored"
}
someday "block" {}
`)
	if len(got) != 1 || got[0]["version"] != "" || len(got[0]["hashes"].([]any)) != 0 {
		t.Errorf("got %v", got)
	}
}

func TestConvert_Empty(t *testing.T) {
	for _, src := range []string{"", "# nothing locked yet\n"} {
		if got := decode(t, src); len(got) != 0 {
			t.Errorf("convert(%q) = %v, want no providers", src, got)
		}
	}
}

func TestConvert_SyntaxErrors(t *testing.T) {
	for src, fragment := range map[string]string{
		"provider \"registry.opentofu.org/a/b\" {\n  version = \"1\"\n": "test.hcl:",
		"this is { not a lock file":                                     "test.hcl:1",
		"provider \"registry.opentofu.org/a/b\" { version = [\"x\"] }":  "test.hcl:1",
		"provider { version = \"1\" }":                                  "test.hcl:1",
	} {
		var out strings.Builder
		err := convert([]byte(src), "test.hcl", &out)
		if err == nil || !strings.Contains(err.Error(), fragment) {
			t.Errorf("convert(%q) error = %v, want one naming %q", src, err, fragment)
		}
	}
}
