package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/benbirt/rules_tofu/tf/private/cmd/internal/tfconfig"
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
		sourceFile("@@m+//vpc:main.tf", "/ext/m+/vpc/main.tf") + "\n" +
		`{"type":"RULE","rule":{"name":"@@dep+//lib:lib","ruleClass":"_tf_library","location":"/ext/dep+/lib/BUILD.bazel:3:11"}}` + "\n" +
		`{"type":"PACKAGE_GROUP","packageGroup":{"name":"//:pkgs"}}` + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []target{
		{Label: "//:versions.tf", Repo: "", Package: "", Name: "versions.tf", Path: "/ws/versions.tf"},
		{Label: "//net:vpc/extra.tf", Repo: "", Package: "net", Name: "vpc/extra.tf", Path: "/ws/net/vpc/extra.tf"},
		{Label: "@@//dash:main.tf", Repo: "", Package: "dash", Name: "main.tf", Path: "/ws/dash/main.tf"},
		{Label: "@@m+//vpc:main.tf", Repo: "m+", Package: "vpc", Name: "main.tf", Path: "/ext/m+/vpc/main.tf"},
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

// fakeBazel writes a bazel stand-in whose query prints the given
// main-repository source files of ws.
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
// targets (see sourceFile).
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
    ;;
esac
exit 0
`)
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path, logPath
}

// lockedProviders decodes the scratch main.tf.json that fakeTofu copied into
// the lock, returning required_providers as local name to version string. It
// fails the test if the configuration contains anything else.
func lockedProviders(t *testing.T, lock []byte) map[string]string {
	t.Helper()
	var doc struct {
		Terraform struct {
			RequiredProviders map[string]tfconfig.Requirement `json:"required_providers"`
		} `json:"terraform"`
	}
	dec := json.NewDecoder(strings.NewReader(string(lock)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("scratch configuration %s: %v", lock, err)
	}
	versions := map[string]string{}
	for name, req := range doc.Terraform.RequiredProviders {
		versions[name] = req.Version
	}
	return versions
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
	got := lockedProviders(t, lock)
	if want := map[string]string{"null": "3.2.4, >= 3.0"}; !reflect.DeepEqual(got, want) {
		t.Errorf("locked against %q, want %q: the versions.tf pin and the library floor", got, want)
	}

	tofuCalls, _ := os.ReadFile(logPath)
	lines := strings.Split(strings.TrimSpace(string(tofuCalls)), "\n")
	if len(lines) != 1 || !strings.HasSuffix(lines[0], "providers lock -platform=linux_amd64 -platform=darwin_arm64") {
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
	got := lockedProviders(t, lock)
	if want := map[string]string{"null": ">= 3.0"}; !reflect.DeepEqual(got, want) {
		t.Errorf("locked against %q, want %q", got, want)
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
	got := lockedProviders(t, lock)
	if want := map[string]string{"null": "3.2.4"}; !reflect.DeepEqual(got, want) {
		t.Errorf("locked against %q, want %q", got, want)
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
	got := lockedProviders(t, lock)
	if want := map[string]string{"null": ">= 3.0"}; !reflect.DeepEqual(got, want) {
		t.Errorf("locked against %q, want only hashicorp/null: %q", got, want)
	}
	if !strings.Contains(log.String(), "from the required_providers of 1 directory\n") {
		t.Errorf("a directory left with no requirements shouldn't be counted:\n%s", log.String())
	}
}

// A module from another repository is locked against in every directory the
// query returns a configuration file for: those are its library's srcs,
// whose providers the library lists, called or not. Its examples are not
// among them, because tf_modules leaves them out of the library.
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
	// Outside modules/: in the library because its tag names it in subdirs.
	writeFile(t, filepath.Join(ext, "vpc/peering/main.tf"), `module "tools" { source = "github.com/acme/other//tools" }
terraform {
  required_providers {
    dns = { source = "hashicorp/dns" }
  }
}
`)
	// Named in subdirs too, and called by nothing: its provider is on the
	// library all the same, so it is locked.
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
	// examples/complete/main.tf is on disk and in no target, so not in the query.
	for _, f := range []string{"vpc:main.tf", "vpc:modules/sub/main.tf", "vpc:peering/main.tf", "vpc:uncalled/main.tf", ":shared/main.tf"} {
		pkg, name, _ := strings.Cut(f, ":")
		targets = append(targets, sourceFile(repo+"//"+f, filepath.Join(ext, pkg, name)))
	}
	for _, f := range []string{"main.tf", "tools/main.tf"} {
		targets = append(targets, sourceFile(otherRepo+"//:"+f, filepath.Join(other, f)))
	}
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
	got := lockedProviders(t, lock)
	want := map[string]string{"null": ">= 3.0, >= 3.1", "time": "", "tls": "", "dns": "", "local": "", "external": "", "http": ""}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("locked against %q, want %q", got, want)
	}
	if !strings.Contains(log.String(), "from the required_providers of 8 directories\n") {
		t.Errorf("log = %q", log.String())
	}
}

// When every required provider is one the root module tags, there is nothing
// to lock: pin writes an empty lock (tofu's header, no providers) over the old
// one without running tofu, so a stale entry can't conflict with the tag.
func TestRun_AllProvidersTagged(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "lib/main.tf"), `terraform {
  required_providers {
    null = { source = "hashicorp/null", version = "3.2.4" }
  }
}
`)
	writeFile(t, filepath.Join(ws, ".terraform.lock.hcl"), "# old\nprovider \"registry.opentofu.org/hashicorp/null\" {\n  version = \"3.2.3\"\n}\n")
	bazel := fakeBazel(t, ws, "//lib:main.tf")
	tofu, logPath := fakeTofu(t)

	var log strings.Builder
	if err := run(ws, "", tofu, bazel, nil, []string{"hashicorp/null"}, nil, &log); err != nil {
		t.Fatalf("run: %v\n%s", err, log.String())
	}
	lock, err := os.ReadFile(filepath.Join(ws, ".terraform.lock.hcl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(lock) != emptyLock {
		t.Errorf("lock = %q, want the empty lock %q", lock, emptyLock)
	}
	if !strings.Contains(log.String(), "every required provider is pinned by a tag") {
		t.Errorf("expected a note that nothing was left to lock, got\n%s", log.String())
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Errorf("tofu should not have run, got err=%v", err)
	}
}

// requiring writes a file that requires the given `<local name> = <entry>`
// lines.
func requiring(t *testing.T, path string, entries ...string) string {
	t.Helper()
	writeFile(t, path, "terraform {\n  required_providers {\n    "+strings.Join(entries, "\n    ")+"\n  }\n}\n")
	return path
}

// The files of a directory may belong to several root modules (issue #99),
// so its requirements are combined by provider source, never by local name.
func TestRequirements(t *testing.T) {
	const aws, acme, null = "registry.opentofu.org/hashicorp/aws", "registry.opentofu.org/acme/aws", "registry.opentofu.org/hashicorp/null"
	for _, tc := range []struct {
		name  string
		files map[string][]string
		want  []tfconfig.Requirement
	}{
		{
			name: "one name and source, two constraints",
			files: map[string][]string{
				"dev.tf":  {`aws = { source = "hashicorp/aws", version = "~> 5.0" }`},
				"prod.tf": {`aws = { source = "hashicorp/aws", version = ">= 5.40" }`},
			},
			want: []tfconfig.Requirement{{Source: aws, Version: ">= 5.40, ~> 5.0"}},
		},
		{
			name: "one name, two sources",
			files: map[string][]string{
				"dev.tf":  {`aws = { source = "hashicorp/aws", version = "~> 5.0" }`},
				"prod.tf": {`aws = { source = "acme/aws", version = "1.2.3" }`},
			},
			want: []tfconfig.Requirement{{Source: acme, Version: "1.2.3"}, {Source: aws, Version: "~> 5.0"}},
		},
		{
			name: "the same constraint twice is written once",
			files: map[string][]string{
				"dev.tf":  {`aws = { source = "hashicorp/aws", version = "~> 5.0" }`},
				"prod.tf": {`aws = { source = "hashicorp/aws", version = "~> 5.0" }`},
			},
			want: []tfconfig.Requirement{{Source: aws, Version: "~> 5.0"}},
		},
		{
			name: "two names for one source",
			files: map[string][]string{
				"main.tf": {`aws = { source = "hashicorp/aws", version = "~> 5.0" }`, `amazon = { source = "HashiCorp/AWS", version = "< 5.90" }`},
			},
			want: []tfconfig.Requirement{{Source: aws, Version: "< 5.90, ~> 5.0"}},
		},
		{
			name: "no version constrains nothing",
			files: map[string][]string{
				"dev.tf":  {`aws = { source = "hashicorp/aws" }`},
				"prod.tf": {`aws = { source = "hashicorp/aws", version = "~> 5.0" }`},
				"null.tf": {`null = { source = "hashicorp/null" }`},
			},
			want: []tfconfig.Requirement{{Source: aws, Version: "~> 5.0"}, {Source: null}},
		},
		{
			name: "an override replaces every entry of its name",
			files: map[string][]string{
				"dev.tf":          {`aws = { source = "hashicorp/aws", version = "~> 5.0" }`, `null = { source = "hashicorp/null", version = "3.2.4" }`},
				"prod.tf":         {`aws = { source = "hashicorp/aws", version = ">= 5.40" }`},
				"pin_override.tf": {`aws = { source = "hashicorp/aws", version = "5.50.0" }`},
			},
			want: []tfconfig.Requirement{{Source: aws, Version: "5.50.0"}, {Source: null, Version: "3.2.4"}},
		},
		{
			name: "a later override replaces an earlier one, source included",
			files: map[string][]string{
				"main.tf":       {`aws = { source = "hashicorp/aws", version = "~> 5.0" }`},
				"a_override.tf": {`aws = { source = "hashicorp/aws", version = "5.50.0" }`},
				"b_override.tf": {`aws = { source = "acme/aws", version = "1.2.3" }`},
			},
			want: []tfconfig.Requirement{{Source: acme, Version: "1.2.3"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			var files []string
			for name, entries := range tc.files {
				files = append(files, requiring(t, filepath.Join(dir, name), entries...))
			}
			got, err := requirements(files)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("requirements = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestLocalNames(t *testing.T) {
	got := localNames([]tfconfig.Requirement{
		{Source: "registry.opentofu.org/acme/aws"},
		{Source: "registry.opentofu.org/hashicorp/aws"},
		{Source: "registry.opentofu.org/hashicorp/null"},
		{Source: "example.com/other/aws"},
		{Source: "registry.opentofu.org/acme/aws-2"},
	})
	if want := []string{"aws", "aws-2", "null", "aws-3", "aws-2-2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("localNames = %q, want %q", got, want)
	}
	// tofu refuses a required_providers entry under any other kind of name
	// ("must contain only letters, digits, and dashes, and may not use
	// leading or trailing dashes"), and the fake tofu of these tests would
	// not notice.
	valid := regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	for _, name := range got {
		if !valid.MatchString(name) {
			t.Errorf("%q is not a provider local name tofu accepts", name)
		}
	}
}

// Two tf_deploys in one package, each with its own srcs, each requiring the
// provider: pin sees one directory with two entries named `null`, which must
// lock against both constraints rather than fail (issue #99). A second
// provider under the same local name in another pair of files is locked too.
func TestRun_TwoTargetsOneDirectory(t *testing.T) {
	ws := t.TempDir()
	requiring(t, filepath.Join(ws, "stacks/dev.tf"), `null = { source = "hashicorp/null", version = "~> 3.2" }`)
	requiring(t, filepath.Join(ws, "stacks/prod.tf"), `null = { source = "hashicorp/null", version = ">= 3.2.3" }`)
	requiring(t, filepath.Join(ws, "mixed/a.tf"), `aws = { source = "hashicorp/aws", version = "~> 5.0" }`)
	requiring(t, filepath.Join(ws, "mixed/b.tf"), `aws = { source = "acme/aws", version = "1.2.3" }`)
	bazel := fakeBazel(t, ws, "//stacks:dev.tf", "//stacks:prod.tf", "//mixed:a.tf", "//mixed:b.tf")
	tofu, _ := fakeTofu(t)

	var log strings.Builder
	if err := run(ws, "", tofu, bazel, []string{"linux_amd64"}, nil, nil, &log); err != nil {
		t.Fatalf("run: %v\n%s", err, log.String())
	}
	lock, err := os.ReadFile(filepath.Join(ws, ".terraform.lock.hcl"))
	if err != nil {
		t.Fatal(err)
	}
	got := lockedProviders(t, lock)
	// combine sorts by source, so aws is acme/aws and aws-2 is hashicorp/aws.
	want := map[string]string{"aws": "1.2.3", "aws-2": "~> 5.0", "null": ">= 3.2.3, ~> 3.2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("locked against %q, want %q", got, want)
	}

	// The summary printed when tofu can't meet the constraints shows the
	// combined one.
	t.Setenv("FAKE_TOFU_FAIL", "1")
	log.Reset()
	if err := run(ws, "", tofu, bazel, []string{"linux_amd64"}, nil, nil, &log); err == nil {
		t.Fatal("expected run to fail")
	}
	if want := "stacks  >= 3.2.3, ~> 3.2"; !strings.Contains(log.String(), want) {
		t.Errorf("log lacks %q:\n%s", want, log.String())
	}
}

// Across directories, requirements combine by canonical provider source
// regardless of local name. Duplicate version strings appear once. An entry
// without a version adds no constraint and still counts toward the directory
// total.
func TestRun_CombinesDirectories(t *testing.T) {
	ws := t.TempDir()
	requiring(t, filepath.Join(ws, "a/main.tf"), `null = { source = "hashicorp/null", version = "3.2.3" }`)
	requiring(t, filepath.Join(ws, "b/main.tf"), `nothing = { source = "HashiCorp/null" }`)
	requiring(t, filepath.Join(ws, "c/main.tf"), `null = { source = "hashicorp/null", version = ">= 3.0" }`)
	requiring(t, filepath.Join(ws, "d/main.tf"), `null = { source = "registry.opentofu.org/hashicorp/null", version = ">= 3.0" }`)
	bazel := fakeBazel(t, ws, "//a:main.tf", "//b:main.tf", "//c:main.tf", "//d:main.tf")
	tofu, _ := fakeTofu(t)

	var log strings.Builder
	if err := run(ws, "", tofu, bazel, []string{"linux_amd64"}, nil, nil, &log); err != nil {
		t.Fatalf("run: %v\n%s", err, log.String())
	}
	lock, err := os.ReadFile(filepath.Join(ws, ".terraform.lock.hcl"))
	if err != nil {
		t.Fatal(err)
	}
	got := lockedProviders(t, lock)
	if want := map[string]string{"null": "3.2.3, >= 3.0"}; !reflect.DeepEqual(got, want) {
		t.Errorf("locked against %q, want %q", got, want)
	}
	if !strings.Contains(log.String(), "from the required_providers of 4 directories\n") {
		t.Errorf("log = %q", log.String())
	}
}
