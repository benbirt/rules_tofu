package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const nullFloor = `terraform {
  required_providers {
    null = { source = "hashicorp/null", version = ">= 3.0" }
  }
}
`

func TestDiscover_SkipsHiddenIgnoredAndNestedWorkspaces(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "MODULE.bazel"), "")
	writeFile(t, filepath.Join(ws, "versions.tf"), nullFloor)
	writeFile(t, filepath.Join(ws, "net/vpc/main.tf"), nullFloor)
	writeFile(t, filepath.Join(ws, "dash/main.tf.json"), `{}`)
	writeFile(t, filepath.Join(ws, "docs/README.md"), "")
	writeFile(t, filepath.Join(ws, ".git/hooks/x.tf"), nullFloor)
	writeFile(t, filepath.Join(ws, "net/.terraform/modules/m/main.tf"), nullFloor)
	writeFile(t, filepath.Join(ws, "e2e/smoke/MODULE.bazel"), "")
	writeFile(t, filepath.Join(ws, "e2e/smoke/main.tf"), nullFloor)
	writeFile(t, filepath.Join(ws, "vendor/thing/main.tf"), nullFloor)
	writeFile(t, filepath.Join(ws, ".bazelignore"), "# comment\nvendor/thing/\n")
	if err := os.Symlink(filepath.Join(ws, "net"), filepath.Join(ws, "bazel-out")); err != nil {
		t.Fatal(err)
	}

	got, err := discover(ws)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"", "dash", "net/vpc"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("discover = %q, want %q", got, want)
	}
}

func TestParseDir_Forms(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "main.tf"), `
terraform {
  required_version = ">= 1.6"
  required_providers {
    aws = {
      source                = "hashicorp/aws"
      version               = ">= 6.0"
      configuration_aliases = [aws.east]
    }
    google = "~> 7.0" # legacy form: source implied
    ph = { source = "PostHog/posthog" }
  }
}

resource "null_resource" "x" {
  triggers = { a = var.undeclared }
}
`)
	writeFile(t, filepath.Join(dir, "extra.tf.json"), `{"terraform": {"required_providers": {"random": {"source": "hashicorp/random", "version": "3.6.0"}}}}`)

	got, err := parseDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]requirement{
		"aws":    {Source: "hashicorp/aws", Version: ">= 6.0"},
		"google": {Source: "hashicorp/google", Version: "~> 7.0"},
		"ph":     {Source: "PostHog/posthog"},
		"random": {Source: "hashicorp/random", Version: "3.6.0"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseDir = %+v, want %+v", got, want)
	}
}

func TestParseDir_DuplicateLocalName(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.tf"), nullFloor)
	writeFile(t, filepath.Join(dir, "b.tf"), nullFloor)
	if _, err := parseDir(dir); err == nil || !strings.Contains(err.Error(), "already required") {
		t.Errorf("expected a duplicate error, got %v", err)
	}
}

func TestModuleNames(t *testing.T) {
	got := moduleNames([]module{{Dir: "examples/lock/greeting"}, {Dir: "1st"}, {Dir: "examples/lock.greeting"}})
	want := []string{"examples_lock_greeting", "m_1st", "examples_lock_greeting_2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("moduleNames = %q, want %q", got, want)
	}
}

// fakeTofu writes a tofu stand-in that logs its arguments, copies the root
// module it is pointed at into the lock (so tests can see the scratch
// configuration), and fails `providers lock` when FAKE_TOFU_FAIL is set.
func fakeTofu(t *testing.T) (path, logPath string) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, "tofu")
	logPath = filepath.Join(dir, "log")
	writeFile(t, path, `#!/bin/sh
echo "$@" >> "`+logPath+`"
chdir=${1#-chdir=}
case "$2 $3" in
  "providers lock")
    [ -n "$FAKE_TOFU_FAIL" ] && { echo "no available releases match" >&2; exit 1; }
    cat "$chdir/main.tf.json" > "$chdir/.terraform.lock.hcl"
    for m in "$chdir"/modules/*/main.tf.json; do [ -f "$m" ] && cat "$m" >> "$chdir/.terraform.lock.hcl"; done
    ;;
esac
exit 0
`)
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path, logPath
}

func TestRun_LocksAgainstEveryDirectory(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "infra/versions.tf"), `terraform {
  required_providers {
    null = { source = "hashicorp/null", version = "3.2.4" }
  }
}
`)
	writeFile(t, filepath.Join(ws, "infra/net/main.tf"), nullFloor)
	tofu, logPath := fakeTofu(t)

	var log strings.Builder
	if err := run(ws, "infra", tofu, []string{"linux_amd64", "darwin_arm64"}, nil, nil, &log); err != nil {
		t.Fatalf("run: %v\n%s", err, log.String())
	}

	lock, err := os.ReadFile(filepath.Join(ws, "infra/.terraform.lock.hcl"))
	if err != nil {
		t.Fatal(err)
	}
	// The fake lock is the root module then each child module.
	dec := json.NewDecoder(strings.NewReader(string(lock)))
	var rootDoc, childDoc map[string]any
	if err := dec.Decode(&rootDoc); err != nil {
		t.Fatal(err)
	}
	if err := dec.Decode(&childDoc); err != nil {
		t.Fatal(err)
	}
	if got := rootDoc["terraform"].(map[string]any)["required_providers"].(map[string]any)["null"].(map[string]any)["version"]; got != "3.2.4" {
		t.Errorf("root module pins %v, want 3.2.4", got)
	}
	if got := rootDoc["module"].(map[string]any)["infra_net"].(map[string]any)["source"]; got != "./modules/infra_net" {
		t.Errorf("child module source = %v", got)
	}
	if got := childDoc["terraform"].(map[string]any)["required_providers"].(map[string]any)["null"].(map[string]any)["version"]; got != ">= 3.0" {
		t.Errorf("child module requires %v, want >= 3.0", got)
	}

	calls, _ := os.ReadFile(logPath)
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], " get") ||
		!strings.HasSuffix(lines[1], "providers lock -platform=linux_amd64 -platform=darwin_arm64") {
		t.Errorf("tofu calls = %q", lines)
	}
	if !strings.Contains(log.String(), "from the required_providers of 2 directories") {
		t.Errorf("log = %q", log.String())
	}
}

func TestRun_NoVersionsTF(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "a/main.tf"), nullFloor)
	writeFile(t, filepath.Join(ws, "b/main.tf"), nullFloor)
	tofu, logPath := fakeTofu(t)

	var log strings.Builder
	if err := run(ws, "", tofu, []string{"linux_amd64"}, nil, []string{"-platform=windows_amd64"}, &log); err != nil {
		t.Fatalf("run: %v\n%s", err, log.String())
	}
	lock, err := os.ReadFile(filepath.Join(ws, ".terraform.lock.hcl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.SplitN(string(lock), "\n}\n", 2)[0], "required_providers") {
		t.Errorf("root module should have no requirements of its own:\n%s", lock)
	}
	calls, _ := os.ReadFile(logPath)
	if !strings.Contains(string(calls), "providers lock -platform=windows_amd64\n") {
		t.Errorf("explicit -platform should replace the defaults, got %q", calls)
	}
}

func TestRun_ConflictListsConstraints(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "versions.tf"), `terraform {
  required_providers {
    null = { source = "hashicorp/null", version = "2.1.0" }
  }
}
`)
	writeFile(t, filepath.Join(ws, "net/main.tf"), nullFloor)
	writeFile(t, filepath.Join(ws, ".terraform.lock.hcl"), "# old\n")
	tofu, _ := fakeTofu(t)
	t.Setenv("FAKE_TOFU_FAIL", "1")

	var log strings.Builder
	err := run(ws, "", tofu, []string{"linux_amd64"}, nil, nil, &log)
	if err == nil {
		t.Fatal("expected run to fail")
	}
	for _, want := range []string{"hashicorp/null", "(workspace root)  2.1.0", "net               >= 3.0"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, log.String())
		}
	}
	if lock, _ := os.ReadFile(filepath.Join(ws, ".terraform.lock.hcl")); string(lock) != "# old\n" {
		t.Errorf("a failed pin must leave the lock alone, got %q", lock)
	}
}

func TestRun_NothingToLock(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "main.tf"), `output "x" { value = 1 }`)
	tofu, _ := fakeTofu(t)
	if err := run(ws, "", tofu, nil, nil, nil, &strings.Builder{}); err == nil || !strings.Contains(err.Error(), "no required_providers") {
		t.Errorf("expected a nothing-to-lock error, got %v", err)
	}
}

func TestRun_ExcludesTaggedProviders(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "lib/main.tf"), `terraform {
  required_providers {
    null = { source = "hashicorp/null", version = ">= 3.0" }
    ph   = { source = "registry.opentofu.org/PostHog/posthog" }
  }
}
`)
	writeFile(t, filepath.Join(ws, "tagged/main.tf"), `terraform {
  required_providers {
    ph = { source = "PostHog/posthog", version = "1.0.24" }
  }
}
`)
	tofu, _ := fakeTofu(t)

	var log strings.Builder
	if err := run(ws, "", tofu, nil, []string{"posthog/posthog"}, nil, &log); err != nil {
		t.Fatalf("run: %v\n%s", err, log.String())
	}
	lock, _ := os.ReadFile(filepath.Join(ws, ".terraform.lock.hcl"))
	if strings.Contains(strings.ToLower(string(lock)), "posthog") || !strings.Contains(string(lock), "hashicorp/null") {
		t.Errorf("expected only hashicorp/null to be locked, got:\n%s", lock)
	}
	if strings.Contains(string(lock), "tagged") {
		t.Errorf("a directory left with no requirements shouldn't become a module:\n%s", lock)
	}
}
