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

func TestGroupByDir(t *testing.T) {
	got := groupByDir(`//:versions.tf
//net/vpc:main.tf
//net/vpc:outputs.tf.json
//net:vpc/extra.tf
@@//dash:main.tf
@other//x:main.tf
`)
	want := map[string][]string{
		"":        {"versions.tf"},
		"net/vpc": {"net/vpc/main.tf", "net/vpc/outputs.tf.json", "net/vpc/extra.tf"},
		"dash":    {"dash/main.tf"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("groupByDir = %q, want %q", got, want)
	}
}

func TestParseFiles_Forms(t *testing.T) {
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

	writeFile(t, filepath.Join(dir, "unlisted.tf"), nullFloor)

	got, err := parseFiles(dir, []string{"main.tf", "extra.tf.json"})
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

func TestParseFiles_DuplicateLocalName(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.tf"), nullFloor)
	writeFile(t, filepath.Join(dir, "b.tf"), nullFloor)
	if _, err := parseFiles(dir, []string{"a.tf", "b.tf"}); err == nil || !strings.Contains(err.Error(), "already required") {
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

// fakeBazel writes a bazel stand-in whose `query` prints labels, one per line.
func fakeBazel(t *testing.T, labels ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bazel")
	writeFile(t, path, "#!/bin/sh\n[ \"$1\" = query ] || exit 2\ncat <<'EOF'\n"+strings.Join(labels, "\n")+"\nEOF\n")
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
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

// lockedModules reads the fake lock back: the scratch root module, then each
// child module's required_providers.
func lockedModules(t *testing.T, lock []byte) (root map[string]any, children []map[string]any) {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(string(lock)))
	if err := dec.Decode(&root); err != nil {
		t.Fatal(err)
	}
	for dec.More() {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			t.Fatal(err)
		}
		children = append(children, doc["terraform"].(map[string]any)["required_providers"].(map[string]any))
	}
	return root, children
}

func TestRun_LocksAgainstEveryRule(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "infra/versions.tf"), `terraform {
  required_providers {
    null = { source = "hashicorp/null", version = "3.2.4" }
  }
}
`)
	writeFile(t, filepath.Join(ws, "infra/net/main.tf"), nullFloor)
	// In no rule, so not queried and not locked against.
	writeFile(t, filepath.Join(ws, "scratch/main.tf"), `terraform {
  required_providers {
    null = { source = "hashicorp/null", version = "< 3.0" }
  }
}
`)
	bazel := fakeBazel(t, "//infra:versions.tf", "//infra/net:main.tf")
	tofu, logPath := fakeTofu(t)

	var log strings.Builder
	if err := run(ws, "infra", tofu, bazel, []string{"linux_amd64", "darwin_arm64"}, nil, nil, &log); err != nil {
		t.Fatalf("run: %v\n%s", err, log.String())
	}

	lock, err := os.ReadFile(filepath.Join(ws, "infra/.terraform.lock.hcl"))
	if err != nil {
		t.Fatal(err)
	}
	root, children := lockedModules(t, lock)
	calls := root["module"].(map[string]any)
	if len(calls) != 2 || calls["infra"].(map[string]any)["source"] != "./modules/infra" ||
		calls["infra_net"].(map[string]any)["source"] != "./modules/infra_net" {
		t.Errorf("module calls = %v", calls)
	}
	var versions []string
	for _, reqs := range children {
		versions = append(versions, reqs["null"].(map[string]any)["version"].(string))
	}
	if !reflect.DeepEqual(versions, []string{"3.2.4", ">= 3.0"}) {
		t.Errorf("locked against %q, want the versions.tf pin and the library floor", versions)
	}

	tofuCalls, _ := os.ReadFile(logPath)
	lines := strings.Split(strings.TrimSpace(string(tofuCalls)), "\n")
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
	bazel := fakeBazel(t, "//a:main.tf", "//b:main.tf")
	tofu, logPath := fakeTofu(t)

	var log strings.Builder
	if err := run(ws, "", tofu, bazel, []string{"linux_amd64"}, nil, []string{"-platform=windows_amd64"}, &log); err != nil {
		t.Fatalf("run: %v\n%s", err, log.String())
	}
	lock, err := os.ReadFile(filepath.Join(ws, ".terraform.lock.hcl"))
	if err != nil {
		t.Fatal(err)
	}
	if _, children := lockedModules(t, lock); len(children) != 2 {
		t.Errorf("expected a module per library, got %d", len(children))
	}
	tofuCalls, _ := os.ReadFile(logPath)
	if !strings.Contains(string(tofuCalls), "providers lock -platform=windows_amd64\n") {
		t.Errorf("explicit -platform should replace the defaults, got %q", tofuCalls)
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
	bazel := fakeBazel(t, "//:versions.tf", "//net:main.tf")
	tofu, _ := fakeTofu(t)
	t.Setenv("FAKE_TOFU_FAIL", "1")

	var log strings.Builder
	err := run(ws, "", tofu, bazel, []string{"linux_amd64"}, nil, nil, &log)
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
	bazel := fakeBazel(t, "//:main.tf")
	tofu, _ := fakeTofu(t)
	if err := run(ws, "", tofu, bazel, nil, nil, nil, &strings.Builder{}); err == nil || !strings.Contains(err.Error(), "declares required_providers") {
		t.Errorf("expected a nothing-to-lock error, got %v", err)
	}
}

func TestRun_QueryFailure(t *testing.T) {
	ws := t.TempDir()
	bazel := filepath.Join(t.TempDir(), "bazel")
	writeFile(t, bazel, "#!/bin/sh\necho 'ERROR: no such package' >&2\nexit 7\n")
	if err := os.Chmod(bazel, 0o755); err != nil {
		t.Fatal(err)
	}
	tofu, _ := fakeTofu(t)
	var log strings.Builder
	err := run(ws, "", tofu, bazel, nil, nil, nil, &log)
	if err == nil || !strings.Contains(err.Error(), "bazel query") || !strings.Contains(log.String(), "no such package") {
		t.Errorf("expected the query's failure to surface, got %v\n%s", err, log.String())
	}
}

// Before the first pin, the providers the rules name have no package in the
// hub, so the query only partly succeeds. Its results still count.
func TestRun_PartialQuery(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "main.tf"), `terraform {
  required_providers {
    null = { source = "hashicorp/null", version = "3.2.4" }
  }
}
`)
	bazel := filepath.Join(t.TempDir(), "bazel")
	writeFile(t, bazel, "#!/bin/sh\necho \"ERROR: no such package '@@tf_providers//hashicorp/null'\" >&2\necho '//:main.tf'\nexit 3\n")
	if err := os.Chmod(bazel, 0o755); err != nil {
		t.Fatal(err)
	}
	tofu, _ := fakeTofu(t)
	var log strings.Builder
	if err := run(ws, "", tofu, bazel, nil, nil, nil, &log); err != nil {
		t.Fatalf("run: %v\n%s", err, log.String())
	}
	lock, err := os.ReadFile(filepath.Join(ws, ".terraform.lock.hcl"))
	if err != nil {
		t.Fatal(err)
	}
	if _, children := lockedModules(t, lock); len(children) != 1 || children[0]["null"] == nil {
		t.Errorf("expected the lock to cover hashicorp/null, got %v", children)
	}
	if !strings.Contains(log.String(), "couldn't load everything") {
		t.Errorf("expected a note about the partial query, got\n%s", log.String())
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
	bazel := fakeBazel(t, "//lib:main.tf", "//tagged:main.tf")
	tofu, _ := fakeTofu(t)

	var log strings.Builder
	if err := run(ws, "", tofu, bazel, nil, []string{"posthog/posthog"}, nil, &log); err != nil {
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
