package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
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

func TestParseTargets(t *testing.T) {
	got, err := parseTargets([]byte(sourceFile("//:versions.tf", "/ws/versions.tf") + "\n" +
		sourceFile("//net:vpc/extra.tf", "/ws/net/vpc/extra.tf") + "\n" +
		sourceFile("@@//dash:main.tf", "/ws/dash/main.tf") + "\n" +
		libraryRule("@@m+//vpc:module", "/ext/m+/vpc/BUILD.bazel", "github.com/acme/m//vpc") + "\n" +
		libraryRule("@@dep+//lib:lib", "/ext/dep+/lib/BUILD.bazel", "") + "\n" +
		`{"type":"PACKAGE_GROUP","packageGroup":{"name":"//:pkgs"}}` + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []target{
		{Kind: "source file", Label: "//:versions.tf", Repo: "", Package: "", Name: "versions.tf", Path: "/ws/versions.tf"},
		{Kind: "source file", Label: "//net:vpc/extra.tf", Repo: "", Package: "net", Name: "vpc/extra.tf", Path: "/ws/net/vpc/extra.tf"},
		{Kind: "source file", Label: "@@//dash:main.tf", Repo: "", Package: "dash", Name: "main.tf", Path: "/ws/dash/main.tf"},
		{Kind: "_tf_library rule", Label: "@@m+//vpc:module", Repo: "m+", Package: "vpc", Name: "module", Path: "/ext/m+/vpc/BUILD.bazel", ModuleSource: "github.com/acme/m//vpc"},
		{Kind: "_tf_library rule", Label: "@@dep+//lib:lib", Repo: "dep+", Package: "lib", Name: "lib", Path: "/ext/dep+/lib/BUILD.bazel"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseTargets = %+v, want %+v", got, want)
	}
	if _, err := parseTargets([]byte("not JSON\n")); err == nil {
		t.Error("expected an error for output that isn't JSON")
	}
}

// sourceFile is a source file as `bazel query --output=streamed_jsonproto`
// prints it.
func sourceFile(label, path string) string {
	return fmt.Sprintf(`{"type":"SOURCE_FILE","sourceFile":{"name":%q,"location":"%s:1:1","visibilityLabel":["//visibility:private"]}}`, label, path)
}

// libraryRule is a tf_library as `bazel query --output=streamed_jsonproto
// --proto:output_rule_attrs=module_source` prints it, declared in buildFile,
// with the given module_source ("" for none).
func libraryRule(label, buildFile, moduleSource string) string {
	attrs := `{"name":"$rule_implementation_hash","type":"STRING","stringValue":"0f95"}`
	if moduleSource != "" {
		attrs = fmt.Sprintf(`{"name":"module_source","type":"STRING","stringValue":%q,"explicitlySpecified":true,"nodep":false},`, moduleSource) + attrs
	}
	return fmt.Sprintf(`{"type":"RULE","rule":{"name":%q,"ruleClass":"_tf_library","location":"%s:3:11","attribute":[%s]}}`, label, buildFile, attrs)
}

func TestModuleNames(t *testing.T) {
	got := moduleNames([]module{{Dir: "examples/lock/greeting"}, {Dir: "1st"}, {Dir: "examples/lock.greeting"}})
	want := []string{"examples_lock_greeting", "m_1st", "examples_lock_greeting_2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("moduleNames = %q, want %q", got, want)
	}
}

// fakeBazel writes a bazel stand-in whose query prints the given
// main-repository source files of ws, and no libraries.
func fakeBazel(t *testing.T, ws string, labels ...string) string {
	t.Helper()
	var lines []string
	for _, label := range labels {
		pkg, name, _ := strings.Cut(strings.TrimPrefix(label, "//"), ":")
		lines = append(lines, sourceFile(label, filepath.Join(ws, pkg, name)))
	}
	return fakeBazelOutput(t, lines...)
}

// fakeBazelOutput writes a bazel stand-in whose query prints the given
// targets (see sourceFile and libraryRule).
func fakeBazelOutput(t *testing.T, targets ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bazel")
	writeFile(t, path, "#!/bin/sh\n[ \"$1\" = query ] || exit 2\ncat <<'EOF'\n"+strings.Join(targets, "\n")+"\nEOF\n")
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
	bazel := fakeBazel(t, ws, "//infra:versions.tf", "//infra/net:main.tf")
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
	bazel := fakeBazel(t, ws, "//a:main.tf", "//b:main.tf")
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
	bazel := fakeBazel(t, ws, "//:versions.tf", "//net:main.tf")
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
	bazel := fakeBazel(t, ws, "//:main.tf")
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
	writeFile(t, bazel, "#!/bin/sh\necho \"ERROR: no such package '@@tf_providers//hashicorp/null'\" >&2\necho '"+sourceFile("//:main.tf", filepath.Join(ws, "main.tf"))+"'\nexit 3\n")
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
	bazel := fakeBazel(t, ws, "//lib:main.tf", "//tagged:main.tf")
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

// A module from another repository is locked against in the directories its
// library serves: its own, its modules/, the submodules outside modules/
// that counted files call with `//<subdir>` (in turn: a call from one of
// those counts too), and what they all call locally. Not in its examples.
func TestRun_ExternalModule(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "app/main.tf"), nullFloor+`
module "peering" {
  source = "git::https://example.com/net.git//vpc/peering?ref=v1"
}
`)
	ext := filepath.Join(t.TempDir(), "rules_tofu++tf_modules+tf_modules_vpc")
	writeFile(t, filepath.Join(ext, "vpc/main.tf"), `module "shared" { source = "../shared" }
terraform {
  required_providers {
    null = { source = "hashicorp/null", version = ">= 3.1" }
  }
}
`)
	// Outside modules/, so served only because app calls it. Its call into
	// the other module's submodule is found once it counts.
	writeFile(t, filepath.Join(ext, "vpc/peering/main.tf"), `module "tools" { source = "github.com/acme/other//tools" }
terraform {
  required_providers {
    dns = { source = "hashicorp/dns" }
  }
}
`)
	writeFile(t, filepath.Join(ext, "vpc/uncalled/main.tf"), `terraform {
  required_providers {
    http = { source = "hashicorp/http" }
  }
}
`)
	other := filepath.Join(t.TempDir(), "rules_tofu++tf_modules+tf_modules_other")
	writeFile(t, filepath.Join(other, "main.tf"), `terraform {
  required_providers {
    external = { source = "hashicorp/external" }
  }
}
`)
	writeFile(t, filepath.Join(other, "tools/main.tf"), `terraform {
  required_providers {
    local = { source = "hashicorp/local" }
  }
}
`)
	writeFile(t, filepath.Join(ext, "shared/main.tf"), `terraform {
  required_providers {
    time = { source = "hashicorp/time" }
  }
}
`)
	writeFile(t, filepath.Join(ext, "vpc/modules/sub/main.tf"), `terraform {
  required_providers {
    tls = { source = "hashicorp/tls" }
  }
}
`)
	writeFile(t, filepath.Join(ext, "vpc/examples/complete/main.tf"), `terraform {
  required_providers {
    random = { source = "hashicorp/random" }
  }
}
`)
	repo := "@@rules_tofu++tf_modules+tf_modules_vpc"
	otherRepo := "@@rules_tofu++tf_modules+tf_modules_other"
	targets := []string{sourceFile("//app:main.tf", filepath.Join(ws, "app/main.tf"))}
	for _, f := range []string{"vpc:main.tf", "vpc:modules/sub/main.tf", "vpc:examples/complete/main.tf", "vpc:peering/main.tf", "vpc:uncalled/main.tf", ":shared/main.tf"} {
		pkg, name, _ := strings.Cut(f, ":")
		targets = append(targets, sourceFile(repo+"//"+f, filepath.Join(ext, pkg, name)))
	}
	for _, f := range []string{"main.tf", "tools/main.tf"} {
		targets = append(targets, sourceFile(otherRepo+"//:"+f, filepath.Join(other, f)))
	}
	targets = append(targets,
		libraryRule(repo+"//vpc:module", filepath.Join(ext, "vpc/BUILD.bazel"), "git::https://example.com/net.git//vpc?ref=v1"),
		libraryRule(otherRepo+"//:module", filepath.Join(other, "BUILD.bazel"), "github.com/acme/other"))
	bazel := fakeBazelOutput(t, targets...)
	tofu, _ := fakeTofu(t)

	var log strings.Builder
	if err := run(ws, "", tofu, bazel, nil, nil, nil, &log); err != nil {
		t.Fatalf("run: %v\n%s", err, log.String())
	}
	lock, err := os.ReadFile(filepath.Join(ws, ".terraform.lock.hcl"))
	if err != nil {
		t.Fatal(err)
	}
	root, children := lockedModules(t, lock)
	locked := map[string]bool{}
	for _, c := range children {
		for name := range c {
			locked[name] = true
		}
	}
	want := map[string]bool{"null": true, "time": true, "tls": true, "dns": true, "local": true, "external": true}
	if !reflect.DeepEqual(locked, want) {
		t.Errorf("locked %v, want %v", locked, want)
	}
	var dirs []string
	for name := range root["module"].(map[string]any) {
		dirs = append(dirs, name)
	}
	sort.Strings(dirs)
	wantDirs := []string{
		"app",
		"m__rules_tofu__tf_modules_tf_modules_other__",
		"m__rules_tofu__tf_modules_tf_modules_other__tools",
		"m__rules_tofu__tf_modules_tf_modules_vpc__shared",
		"m__rules_tofu__tf_modules_tf_modules_vpc__vpc",
		"m__rules_tofu__tf_modules_tf_modules_vpc__vpc_modules_sub",
		"m__rules_tofu__tf_modules_tf_modules_vpc__vpc_peering",
	}
	if !reflect.DeepEqual(dirs, wantDirs) {
		t.Errorf("locked against %q, want %q", dirs, wantDirs)
	}
}

// A library from another Bazel module needs no module_source for its files
// to count: it serves its own directory, and what that calls locally. One
// with no configuration of its own, deps alone, serves nothing.
func TestRun_ExternalLibraryWithoutModuleSource(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "app/main.tf"), nullFloor)
	ext := filepath.Join(t.TempDir(), "acme+")
	writeFile(t, filepath.Join(ext, "lib/main.tf"), `module "util" { source = "./util" }
terraform {
  required_providers {
    tls = { source = "hashicorp/tls" }
  }
}
`)
	writeFile(t, filepath.Join(ext, "lib/util/main.tf"), `terraform {
  required_providers {
    time = { source = "hashicorp/time" }
  }
}
`)
	writeFile(t, filepath.Join(ext, "lib/examples/main.tf"), `terraform {
  required_providers {
    random = { source = "hashicorp/random" }
  }
}
`)
	writeFile(t, filepath.Join(ext, "all/BUILD.bazel"), "")
	bazel := fakeBazelOutput(t,
		sourceFile("//app:main.tf", filepath.Join(ws, "app/main.tf")),
		sourceFile("@@acme+//lib:main.tf", filepath.Join(ext, "lib/main.tf")),
		sourceFile("@@acme+//lib:util/main.tf", filepath.Join(ext, "lib/util/main.tf")),
		sourceFile("@@acme+//lib:examples/main.tf", filepath.Join(ext, "lib/examples/main.tf")),
		libraryRule("@@acme+//lib:lib", filepath.Join(ext, "lib/BUILD.bazel"), ""),
		libraryRule("@@acme+//all:all", filepath.Join(ext, "all/BUILD.bazel"), ""))
	tofu, _ := fakeTofu(t)

	var log strings.Builder
	if err := run(ws, "", tofu, bazel, nil, nil, nil, &log); err != nil {
		t.Fatalf("run: %v\n%s", err, log.String())
	}
	lock, err := os.ReadFile(filepath.Join(ws, ".terraform.lock.hcl"))
	if err != nil {
		t.Fatal(err)
	}
	_, children := lockedModules(t, lock)
	locked := map[string]bool{}
	for _, c := range children {
		for name := range c {
			locked[name] = true
		}
	}
	want := map[string]bool{"null": true, "tls": true, "time": true}
	if !reflect.DeepEqual(locked, want) {
		t.Errorf("locked %v, want %v", locked, want)
	}
}
