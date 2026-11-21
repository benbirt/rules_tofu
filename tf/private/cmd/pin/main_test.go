package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/benbirt/rules_tofu/tf/private/cmd/internal/tofushow"
	"github.com/benbirt/rules_tofu/tf/private/cmd/internal/tofushow/tofutest"
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

const nullSource = "registry.opentofu.org/hashicorp/null"

const nullFloor = `terraform {
  required_providers {
    null = { source = "hashicorp/null", version = ">= 3.0" }
  }
}
`

// fileTarget is a source file as `bazel query --output=streamed_jsonproto`
// prints it.
func fileTarget(label, path string) string {
	return fmt.Sprintf(`{"type":"SOURCE_FILE","sourceFile":{"name":%q,"location":"%s:1:1","visibilityLabel":["//visibility:private"]}}`, label, path)
}

// ruleTarget is a rule as `bazel query --output=streamed_jsonproto
// --proto:output_rule_attrs=srcs,actual,tf_modules_label` prints it: an alias
// has `actual`, any other class has `srcs`.
func ruleTarget(class, label string, srcs ...string) string {
	attribute := map[string]any{"name": "srcs", "type": "LABEL_LIST", "stringListValue": srcs}
	if class == "alias" {
		attribute = map[string]any{"name": "actual", "type": "LABEL", "stringValue": srcs[0]}
	}
	return marshalRule(map[string]any{"name": label, "ruleClass": class, "attribute": []any{attribute}, "ruleInput": srcs})
}

// moduleLibrary is the tf_library that tf_modules generates for a fetched
// module, whose tf_modules_label is its label in the hub.
func moduleLibrary(label, hub string, srcs ...string) string {
	return marshalRule(map[string]any{"name": label, "ruleClass": "_tf_library", "attribute": []any{
		map[string]any{"name": "srcs", "type": "LABEL_LIST", "stringListValue": srcs},
		map[string]any{"name": "tf_modules_label", "type": "STRING", "stringValue": hub},
	}})
}

// generatingRule is a rule with zero or more declared output files.
func generatingRule(class, label string, outputs ...string) string {
	return marshalRule(map[string]any{"name": label, "ruleClass": class, "ruleOutput": outputs})
}

func marshalRule(rule map[string]any) string {
	out, err := json.Marshal(map[string]any{"type": "RULE", "rule": rule})
	if err != nil {
		panic(err)
	}
	return string(out)
}

// library returns query targets for a tf_library named label and its
// main-repository source files under ws.
func library(ws, label string, srcs ...string) []string {
	targets := []string{ruleTarget("_tf_library", label, srcs...)}
	for _, src := range srcs {
		pkg, name, _ := strings.Cut(strings.TrimPrefix(src, "//"), ":")
		targets = append(targets, fileTarget(src, filepath.Join(ws, pkg, name)))
	}
	return targets
}

// fakeBazel writes a bazel stand-in whose query returns the given
// main-repository source files under ws, grouped into one tf_library per
// package.
func fakeBazel(t *testing.T, ws string, labels ...string) string {
	t.Helper()
	byPackage := map[string][]string{}
	var packages []string
	for _, label := range labels {
		pkg, _, _ := strings.Cut(label, ":")
		if byPackage[pkg] == nil {
			packages = append(packages, pkg)
		}
		byPackage[pkg] = append(byPackage[pkg], label)
	}
	var targets []string
	for _, pkg := range packages {
		targets = append(targets, library(ws, pkg+":lib", byPackage[pkg]...)...)
	}
	return fakeBazelOutput(t, targets...)
}

// fakeBazelOutput writes a bazel stand-in whose query prints the given
// targets (see fileTarget and ruleTarget).
func fakeBazelOutput(t *testing.T, targets ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bazel")
	writeFile(t, path, "#!/bin/sh\n[ \"$1\" = query ] || exit 2\ncat <<'EOF'\n"+strings.Join(targets, "\n")+"\nEOF\n")
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeTofu writes a `tofu` stand-in that delegates `show` to the real binary
// and fakes `providers lock` so tests need no registry. It logs `show` calls
// to showLog (one line per call) and all other calls to logPath, exiting 2
// for any command other than `providers lock`. When FAKE_TOFU_FAIL is set,
// `providers lock` fails; otherwise it copies main.tf.json from -chdir= into
// .terraform.lock.hcl so tests can inspect the scratch configuration.
func fakeTofu(t *testing.T) (path, logPath, showLog string) {
	t.Helper()
	real := tofutest.Reader(t, *tofu).Tofu
	dir := t.TempDir()
	path = filepath.Join(dir, "tofu")
	logPath = filepath.Join(dir, "log")
	showLog = filepath.Join(dir, "show.log")
	writeFile(t, path, `#!/bin/sh
if [ "$1" = show ]; then
  echo "$@" >> "`+showLog+`"
  exec "`+real+`" "$@"
fi
echo "$@" >> "`+logPath+`"
chdir=${1#-chdir=}
[ "$2 $3" = "providers lock" ] || exit 2
[ -n "$FAKE_TOFU_FAIL" ] && { echo "no available releases match" >&2; exit 1; }
cat "$chdir/main.tf.json" > "$chdir/.terraform.lock.hcl"
`)
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path, logPath, showLog
}

// lockedProviders decodes the scratch main.tf.json that fakeTofu copied into
// the lock, returning required_providers by local name. It fails the test if
// the configuration contains anything else.
func lockedProviders(t *testing.T, lockPath string) map[string]tofushow.Provider {
	t.Helper()
	lock, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Terraform struct {
			RequiredProviders map[string]tofushow.Provider `json:"required_providers"`
		} `json:"terraform"`
	}
	dec := json.NewDecoder(strings.NewReader(string(lock)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("scratch configuration %s: %v", lock, err)
	}
	return doc.Terraform.RequiredProviders
}

func hashicorp(name string) string {
	return "registry.opentofu.org/hashicorp/" + name
}

func TestParseTargets(t *testing.T) {
	got, err := parseTargets([]byte(fileTarget("//:versions.tf", "/ws/versions.tf") + "\n" +
		fileTarget("//net:vpc/extra.tf", "/ws/net/vpc/extra.tf") + "\n" +
		fileTarget("@@//dash:main.tf", "/ws/dash/main.tf") + "\n" +
		fileTarget("@@m+//vpc:main.tf", "/ext/m+/vpc/main.tf") + "\n" +
		fileTarget("@@m+//:main.tf", "/ext/m+/main.tf") + "\n" +
		ruleTarget("_tf_library", "@@dep+//lib:lib", "@@dep+//lib:main.tf", "@@//dash:main.tf") + "\n" +
		moduleLibrary("@@m+//:_rules_tofu_module", "@tf_modules//m", "@@m+//:main.tf") + "\n" +
		ruleTarget("alias", "//net:alias", "//net:files") + "\n" +
		generatingRule("genrule", "//net:gen", "//net:gen.tf") + "\n" +
		// Captured from Bazel 9.1.0 with --noproto:flatten_selects and
		// --consistent_labels for `srcs = ["a.tf", ":al"] + select({":opt":
		// ["b.tf"], "//conditions:default": [":fg"]})` and
		// `actual = select({":opt": ":fg", "//conditions:default": "e.tf"})`.
		`{"type":"RULE","rule":{"name":"@@//sel:lib","ruleClass":"_tf_library","location":"/ws/sel/BUILD.bazel:9:11","attribute":[{"name":"srcs","type":"SELECTOR_LIST","explicitlySpecified":true,"nodep":false,"selectorList":{"type":"LABEL_LIST","elements":[{"entries":[{"label":"@@//conditions:default","stringListValue":["@@//sel:a.tf","@@//sel:al"],"isDefaultValue":false}],"hasDefaultValue":true,"noMatchError":""},{"entries":[{"label":"@@//sel:opt","stringListValue":["@@//sel:b.tf"],"isDefaultValue":false},{"label":"@@//conditions:default","stringListValue":["@@//sel:fg"],"isDefaultValue":false}],"hasDefaultValue":true,"noMatchError":""}]}},{"name":"tf_modules_label","type":"STRING","stringValue":"","explicitlySpecified":false,"nodep":false},{"name":"$rule_implementation_hash","type":"STRING","stringValue":"d6c0cb8335c8da7bc86c9c8fa28004382f3b7f3869924f36fc1e5e1344179336"}],"ruleInput":["@@//sel:a.tf","@@//sel:al","@@//sel:b.tf","@@//sel:fg","@@//sel:opt"]}}` + "\n" +
		`{"type":"RULE","rule":{"name":"@@//sel:al","ruleClass":"alias","location":"/ws/sel/BUILD.bazel:7:6","attribute":[{"name":"actual","type":"SELECTOR_LIST","explicitlySpecified":true,"nodep":false,"selectorList":{"type":"LABEL","elements":[{"entries":[{"label":"@@//sel:opt","stringValue":"@@//sel:fg","isDefaultValue":false},{"label":"@@//conditions:default","stringValue":"@@//sel:e.tf","isDefaultValue":false}],"hasDefaultValue":true,"noMatchError":""}]}}],"ruleInput":["@@//sel:e.tf","@@//sel:fg","@@//sel:opt"]}}` + "\n" +
		`{"type":"PACKAGE_GROUP","packageGroup":{"name":"//:pkgs"}}` + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := graph{
		files: map[string]sourceFile{
			"//:versions.tf":     {Dir: "", Path: "/ws/versions.tf"},
			"//net:vpc/extra.tf": {Dir: "net/vpc", Path: "/ws/net/vpc/extra.tf"},
			"//dash:main.tf":     {Dir: "dash", Path: "/ws/dash/main.tf"},
			"@@m+//vpc:main.tf":  {Dir: "external/m+/vpc", Path: "/ext/m+/vpc/main.tf"},
			"@@m+//:main.tf":     {Dir: "external/m+", Path: "/ext/m+/main.tf"},
		},
		rules: map[string]rule{
			"@@dep+//lib:lib":           {Class: "_tf_library", Srcs: []element{plain("@@dep+//lib:main.tf", "//dash:main.tf")}},
			"@@m+//:_rules_tofu_module": {Class: "_tf_library", Srcs: []element{plain("@@m+//:main.tf")}, ModulesLabel: "@tf_modules//m"},
			"//net:alias":               {Class: "alias", Actual: []element{plain("//net:files")}},
			"//net:gen":                 {Class: "genrule", Outputs: []string{"//net:gen.tf"}},
			"//sel:lib": {Class: "_tf_library", Srcs: []element{
				{{Condition: "//conditions:default", Labels: []string{"//sel:a.tf", "//sel:al"}}},
				{{Condition: "//sel:opt", Labels: []string{"//sel:b.tf"}}, {Condition: "//conditions:default", Labels: []string{"//sel:fg"}}},
			}},
			"//sel:al": {Class: "alias", Actual: []element{
				{{Condition: "//sel:opt", Labels: []string{"//sel:fg"}}, {Condition: "//conditions:default", Labels: []string{"//sel:e.tf"}}},
			}},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseTargets = %+v, want %+v", got, want)
	}
	if _, err := parseTargets([]byte("not JSON\n")); err == nil {
		t.Error("expected an error for output that isn't JSON")
	}
}

// plain builds a single-branch srcs or actual element without select().
func plain(labels ...string) element {
	return element{{Labels: labels}}
}

// Files are grouped by target and directory through filegroups and aliases,
// merging built outputs with source files and omitting labels that did not
// build, so each group matches what strict deps reads in the build.
func TestGroups(t *testing.T) {
	module := "@@rules_tofu++tf_modules+tf_modules_vpc"
	lines := []string{
		// Two deploys in one package, sharing files through an alias and cyclic
		// filegroups.
		ruleTarget("tf_deploy_rule", "//stacks:dev", "//stacks:dev.tf", "//stacks:shared"),
		ruleTarget("tf_deploy_rule", "//stacks:prod", "//stacks:prod.tf", "//stacks:shared"),
		ruleTarget("alias", "//stacks:shared", "//stacks:files"),
		ruleTarget("filegroup", "//stacks:files", "//stacks:common.tf", "//stacks:nested", "//stacks:README.md"),
		ruleTarget("filegroup", "//stacks:nested", "//stacks:sub/extra.tf.json", "//stacks:files"),
		// A library over two directories, with generated configuration: a
		// declared output named by its label, a rule with a declared
		// configuration output, a rule that declares none, and a rule
		// declaring only other outputs. //missing:main.tf is in a package
		// Bazel could not load, which the query reports itself.
		ruleTarget("_tf_library", "//lib:lib", "//lib:main.tofu", "//lib:modules/a/main.tf", "//lib:gen.tf", "//lib:written", "//lib:template.tftpl", "//lib:rendered", "//lib:docs", "//missing:main.tf"),
		generatingRule("genrule", "//lib:gen", "//lib:gen.tf"),
		generatingRule("write_file", "//lib:written", "//lib:written.tf.json", "//lib:written.txt"),
		generatingRule("render", "//lib:rendered"),
		generatingRule("genrule", "//lib:docs", "//lib:docs.md"),
		// Test files are not configuration.
		ruleTarget("_tf_test", "//lib:test", "//lib:main.tftest.hcl"),
		// A fetched module goes by its tf_modules_label in the hub.
		ruleTarget("alias", "@tf_modules//vpc:vpc", module+"//:_rules_tofu_module"),
		moduleLibrary(module+"//:_rules_tofu_module", "@tf_modules//vpc", module+"//:main.tf", module+"//:modules/sub/main.tf"),
	}
	for label, path := range map[string]string{
		"//stacks:dev.tf":                 "/ws/stacks/dev.tf",
		"//stacks:prod.tf":                "/ws/stacks/prod.tf",
		"//stacks:common.tf":              "/ws/stacks/common.tf",
		"//stacks:sub/extra.tf.json":      "/ws/stacks/sub/extra.tf.json",
		"//lib:main.tofu":                 "/ws/lib/main.tofu",
		"//lib:modules/a/main.tf":         "/ws/lib/modules/a/main.tf",
		module + "//:main.tf":             "/ext/vpc/main.tf",
		module + "//:modules/sub/main.tf": "/ext/vpc/modules/sub/main.tf",
		// Returned by the query and in no target's srcs.
		"//unused:main.tf": "/ws/unused/main.tf",
	} {
		lines = append(lines, fileTarget(label, path))
	}
	g, err := parseTargets([]byte(strings.Join(lines, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	sels := g.selections()
	if got, want := toBuild(sels), []string{"//lib:gen.tf", "//lib:rendered", "//lib:written"}; !reflect.DeepEqual(got, want) {
		t.Errorf("toBuild = %q, want %q", got, want)
	}
	// //lib:written did not build.
	built := map[string][]sourceFile{
		"//lib:gen.tf":   {{Dir: "lib", Path: "/out/lib/gen.tf"}},
		"//lib:rendered": {{Dir: "lib/rendered", Path: "/out/lib/rendered/main.tf"}},
	}
	want := []group{
		{Label: "//lib:lib", Dir: "lib", Files: []string{"/out/lib/gen.tf", "/ws/lib/main.tofu"}},
		{Label: "//lib:lib", Dir: "lib/modules/a", Files: []string{"/ws/lib/modules/a/main.tf"}},
		{Label: "//lib:lib", Dir: "lib/rendered", Files: []string{"/out/lib/rendered/main.tf"}},
		{Label: "//stacks:dev", Dir: "stacks", Files: []string{"/ws/stacks/common.tf", "/ws/stacks/dev.tf"}},
		{Label: "//stacks:dev", Dir: "stacks/sub", Files: []string{"/ws/stacks/sub/extra.tf.json"}},
		{Label: "//stacks:prod", Dir: "stacks", Files: []string{"/ws/stacks/common.tf", "/ws/stacks/prod.tf"}},
		{Label: "//stacks:prod", Dir: "stacks/sub", Files: []string{"/ws/stacks/sub/extra.tf.json"}},
		{Label: "@tf_modules//vpc", Dir: "external/rules_tofu++tf_modules+tf_modules_vpc", Files: []string{"/ext/vpc/main.tf"}},
		{Label: "@tf_modules//vpc", Dir: "external/rules_tofu++tf_modules+tf_modules_vpc/modules/sub", Files: []string{"/ext/vpc/modules/sub/main.tf"}},
	}
	if got := g.groups(sels, built); !reflect.DeepEqual(got, want) {
		t.Errorf("groups =\n%+v, want\n%+v", got, want)
	}
}

// Each combination of select() branches forms its own group: unconditional
// files plus one branch of each select(). select()s with the same condition
// set take the same branch together, as Bazel chooses one condition for both;
// select()s with different condition sets combine in full Cartesian product.
func TestGroups_Select(t *testing.T) {
	const opt, def = "//sel:opt", "//conditions:default"
	const aws, gcp, big = "//cfg:aws", "//cfg:gcp", "//cfg:big"
	g := graph{
		files: map[string]sourceFile{},
		rules: map[string]rule{
			// `["a.tf", ":al"] + select({":opt": ["b.tf"], default: [":fg"]})`,
			// where :al is `alias(actual = select({":opt": ":fg", default:
			// "e.tf"}))`: same condition set, so two groups rather than four.
			"//sel:lib": {Class: "_tf_library", Srcs: []element{
				plain("//sel:a.tf", "//sel:al"),
				{{Condition: opt, Labels: []string{"//sel:b.tf"}}, {Condition: def, Labels: []string{"//sel:fg"}}},
			}},
			"//sel:al": {Class: "alias", Actual: []element{{{Condition: opt, Labels: []string{"//sel:fg"}}, {Condition: def, Labels: []string{"//sel:e.tf"}}}}},
			"//sel:fg": {Class: "filegroup", Srcs: []element{plain("//sel:c.tf", "//sel:d.tf")}},
			// Three select()s: two over {aws, gcp} (in different branch orders)
			// and one over {big, default}, giving four combinations.
			"//multi:lib": {Class: "_tf_library", Srcs: []element{
				plain("//multi:m.tf"),
				{{Condition: aws, Labels: []string{"//multi:aws.tf"}}, {Condition: gcp, Labels: []string{"//multi:gcp.tf"}}},
				{{Condition: big, Labels: []string{"//multi:big.tf"}}, {Condition: def}},
				{{Condition: gcp, Labels: []string{"//multi:gcp2.tf"}}, {Condition: aws, Labels: []string{"//multi:aws2.tf"}}},
			}},
			// A filegroup with two select()s: one includes a generated file in
			// one branch, and the other lists y.tf in both branches.
			// Combinations producing the same files collapse into one group
			// under the first combination.
			"//fg:lib": {Class: "_tf_library", Srcs: []element{plain("//fg:files")}},
			"//fg:files": {Class: "filegroup", Srcs: []element{
				{{Condition: opt, Labels: []string{"//fg:gen.tf"}}, {Condition: def, Labels: []string{"//fg:x.tf"}}},
				{{Condition: aws, Labels: []string{"//fg:y.tf"}}, {Condition: def, Labels: []string{"//fg:y.tf"}}},
			}},
			"//fg:gen": {Class: "genrule", Outputs: []string{"//fg:gen.tf"}},
		},
	}
	for _, label := range []string{
		"//sel:a.tf", "//sel:b.tf", "//sel:c.tf", "//sel:d.tf", "//sel:e.tf",
		"//multi:m.tf", "//multi:aws.tf", "//multi:gcp.tf", "//multi:big.tf", "//multi:aws2.tf", "//multi:gcp2.tf",
		"//fg:x.tf", "//fg:y.tf",
	} {
		pkg, name, _ := strings.Cut(strings.TrimPrefix(label, "//"), ":")
		g.files[label] = sourceFile{Dir: pkg, Path: "/ws/" + pkg + "/" + name}
	}
	sels := g.selections()
	if got, want := toBuild(sels), []string{"//fg:gen.tf"}; !reflect.DeepEqual(got, want) {
		t.Errorf("toBuild = %q, want %q", got, want)
	}
	got := g.groups(sels, map[string][]sourceFile{"//fg:gen.tf": {{Dir: "fg", Path: "/out/fg/gen.tf"}}})
	want := []group{
		{Label: "//fg:lib", Branches: []string{aws, def}, Dir: "fg", Files: []string{"/ws/fg/x.tf", "/ws/fg/y.tf"}},
		{Label: "//fg:lib", Branches: []string{aws, opt}, Dir: "fg", Files: []string{"/out/fg/gen.tf", "/ws/fg/y.tf"}},
		{Label: "//multi:lib", Branches: []string{aws, big}, Dir: "multi", Files: []string{"/ws/multi/aws.tf", "/ws/multi/aws2.tf", "/ws/multi/big.tf", "/ws/multi/m.tf"}},
		{Label: "//multi:lib", Branches: []string{aws, def}, Dir: "multi", Files: []string{"/ws/multi/aws.tf", "/ws/multi/aws2.tf", "/ws/multi/m.tf"}},
		{Label: "//multi:lib", Branches: []string{gcp, big}, Dir: "multi", Files: []string{"/ws/multi/big.tf", "/ws/multi/gcp.tf", "/ws/multi/gcp2.tf", "/ws/multi/m.tf"}},
		{Label: "//multi:lib", Branches: []string{gcp, def}, Dir: "multi", Files: []string{"/ws/multi/gcp.tf", "/ws/multi/gcp2.tf", "/ws/multi/m.tf"}},
		{Label: "//sel:lib", Branches: []string{def}, Dir: "sel", Files: []string{"/ws/sel/a.tf", "/ws/sel/c.tf", "/ws/sel/d.tf", "/ws/sel/e.tf"}},
		{Label: "//sel:lib", Branches: []string{opt}, Dir: "sel", Files: []string{"/ws/sel/a.tf", "/ws/sel/b.tf", "/ws/sel/c.tf", "/ws/sel/d.tf"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("groups =\n%+v, want\n%+v", got, want)
	}
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
	tofu, logPath, _ := fakeTofu(t)

	var log strings.Builder
	if err := run(ws, "infra", tofu, bazel, []string{"linux_amd64", "darwin_arm64"}, nil, nil, &log); err != nil {
		t.Fatalf("run: %v\n%s", err, log.String())
	}

	got := lockedProviders(t, filepath.Join(ws, "infra/.terraform.lock.hcl"))
	if want := map[string]tofushow.Provider{"null": {Source: nullSource, Version: "3.2.4, >= 3.0.0"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("locked against %v, want %v: the versions.tf pin and the library floor", got, want)
	}

	tofuCalls, _ := os.ReadFile(logPath)
	lines := strings.Split(strings.TrimSpace(string(tofuCalls)), "\n")
	if len(lines) != 1 || !strings.HasSuffix(lines[0], "providers lock -platform=linux_amd64 -platform=darwin_arm64") {
		t.Errorf("tofu calls = %q", lines)
	}
	if !strings.Contains(log.String(), "for the providers required in 2 directories") {
		t.Errorf("log = %q", log.String())
	}
}

func TestRun_NoVersionsTF(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "a/main.tf"), nullFloor)
	writeFile(t, filepath.Join(ws, "b/main.tf"), nullFloor)
	bazel := fakeBazel(t, ws, "//a:main.tf", "//b:main.tf")
	tofu, logPath, _ := fakeTofu(t)

	var log strings.Builder
	if err := run(ws, "", tofu, bazel, []string{"linux_amd64"}, nil, []string{"-platform=windows_amd64"}, &log); err != nil {
		t.Fatalf("run: %v\n%s", err, log.String())
	}
	got := lockedProviders(t, filepath.Join(ws, ".terraform.lock.hcl"))
	if want := map[string]tofushow.Provider{"null": {Source: nullSource, Version: ">= 3.0.0"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("locked against %v, want %v", got, want)
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
	writeFile(t, filepath.Join(ws, "net/main.tf"), nullFloor+`resource "random_id" "x" { byte_length = 1 }`)
	writeFile(t, filepath.Join(ws, ".terraform.lock.hcl"), "# old\n")
	bazel := fakeBazel(t, ws, "//:versions.tf", "//net:main.tf")
	tofu, _, _ := fakeTofu(t)
	t.Setenv("FAKE_TOFU_FAIL", "1")

	var log strings.Builder
	err := run(ws, "", tofu, bazel, []string{"linux_amd64"}, nil, nil, &log)
	if err == nil {
		t.Fatal("expected run to fail")
	}
	for _, want := range []string{
		"  registry.opentofu.org/hashicorp/null\n    (workspace root)  2.1.0\n    net               >= 3.0.0\n",
		"  registry.opentofu.org/hashicorp/random\n    net  (any)\n",
	} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, log.String())
		}
	}
	if lock, _ := os.ReadFile(filepath.Join(ws, ".terraform.lock.hcl")); string(lock) != "# old\n" {
		t.Errorf("a failed pin must leave the lock alone, got %q", lock)
	}
}

// Built-in providers have no plugin to lock.
func TestRun_NothingToLock(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "main.tf"), `resource "terraform_data" "x" {}`)
	bazel := fakeBazel(t, ws, "//:main.tf")
	tofu, _, _ := fakeTofu(t)
	if err := run(ws, "", tofu, bazel, nil, nil, nil, &strings.Builder{}); err == nil || !strings.Contains(err.Error(), "requires a provider to lock") {
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
	tofu, _, _ := fakeTofu(t)
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
	writeFile(t, bazel, "#!/bin/sh\necho \"ERROR: no such package '@@tf_providers//hashicorp/null'\" >&2\ncat <<'EOF'\n"+
		strings.Join(library(ws, "//:lib", "//:main.tf"), "\n")+"\nEOF\nexit 3\n")
	if err := os.Chmod(bazel, 0o755); err != nil {
		t.Fatal(err)
	}
	tofu, _, _ := fakeTofu(t)
	var log strings.Builder
	if err := run(ws, "", tofu, bazel, nil, nil, nil, &log); err != nil {
		t.Fatalf("run: %v\n%s", err, log.String())
	}
	got := lockedProviders(t, filepath.Join(ws, ".terraform.lock.hcl"))
	if want := map[string]tofushow.Provider{"null": {Source: nullSource, Version: "3.2.4"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("locked against %v, want %v", got, want)
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
	tofu, _, _ := fakeTofu(t)

	var log strings.Builder
	if err := run(ws, "", tofu, bazel, nil, []string{"posthog/posthog"}, nil, &log); err != nil {
		t.Fatalf("run: %v\n%s", err, log.String())
	}
	got := lockedProviders(t, filepath.Join(ws, ".terraform.lock.hcl"))
	if want := map[string]tofushow.Provider{"null": {Source: nullSource, Version: ">= 3.0.0"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("locked against %v, want only hashicorp/null: %v", got, want)
	}
	if !strings.Contains(log.String(), "for the providers required in 1 directory\n") {
		t.Errorf("a directory left with no requirements shouldn't be counted:\n%s", log.String())
	}
}

// A module from another repository is locked against in every directory its
// library lists files for: the library lists the providers they require,
// called or not. Its examples are not among them, because tf_modules leaves
// them out of the library.
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
	// Only implied, by a resource: the generated library lists it too.
	writeFile(t, filepath.Join(ext, "vpc/modules/sub/main.tf"), `resource "tls_private_key" "k" { algorithm = "ED25519" }`)
	writeFile(t, filepath.Join(ext, "vpc/examples/complete/main.tf"), `terraform {
  required_providers {
    random = { source = "hashicorp/random" }
  }
}
`)
	repo := "@@rules_tofu++tf_modules+tf_modules_vpc"
	otherRepo := "@@rules_tofu++tf_modules+tf_modules_other"
	targets := library(ws, "//app:app", "//app:main.tf")
	// examples/complete/main.tf is on disk and in no target, so not in the query.
	var srcs []string
	for _, f := range []string{"vpc:main.tf", "vpc:modules/sub/main.tf", "vpc:peering/main.tf", "vpc:uncalled/main.tf", ":shared/main.tf"} {
		pkg, name, _ := strings.Cut(f, ":")
		srcs = append(srcs, repo+"//"+f)
		targets = append(targets, fileTarget(repo+"//"+f, filepath.Join(ext, pkg, name)))
	}
	targets = append(targets, ruleTarget("_tf_library", repo+"//vpc:_rules_tofu_module", srcs...))
	srcs = nil
	for _, f := range []string{"main.tf", "tools/main.tf"} {
		srcs = append(srcs, otherRepo+"//:"+f)
		targets = append(targets, fileTarget(otherRepo+"//:"+f, filepath.Join(other, f)))
	}
	targets = append(targets, ruleTarget("_tf_library", otherRepo+"//:_rules_tofu_module", srcs...))
	bazel := fakeBazelOutput(t, targets...)
	tofu, _, _ := fakeTofu(t)

	var log strings.Builder
	if err := run(ws, "", tofu, bazel, nil, nil, nil, &log); err != nil {
		t.Fatalf("run: %v\n%s", err, log.String())
	}
	got := lockedProviders(t, filepath.Join(ws, ".terraform.lock.hcl"))
	want := map[string]tofushow.Provider{"null": {Source: nullSource, Version: ">= 3.0.0, >= 3.1.0"}}
	for _, name := range []string{"time", "tls", "dns", "local", "external", "http"} {
		want[name] = tofushow.Provider{Source: hashicorp(name)}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("locked against %v, want %v", got, want)
	}
	if !strings.Contains(log.String(), "for the providers required in 8 directories\n") {
		t.Errorf("log = %q", log.String())
	}
}

// Providers implied by a resource, data source or provider block (with no
// required_providers entry) are locked, in both workspace targets and fetched
// modules' generated libraries. Built-in providers are skipped.
func TestRun_ImpliedProviders(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "app/main.tf"), `resource "null_resource" "x" {}
data "http" "y" { url = "https://example.com" }
provider "random" {}
resource "terraform_data" "z" {}
`)
	ext := filepath.Join(t.TempDir(), "rules_tofu++tf_modules+tf_modules_m")
	writeFile(t, filepath.Join(ext, "main.tf"), `resource "time_static" "t" {}`)
	repo := "@@rules_tofu++tf_modules+tf_modules_m"
	bazel := fakeBazelOutput(t, append(library(ws, "//app:app", "//app:main.tf"),
		ruleTarget("alias", "@tf_modules//m:m", repo+"//:_rules_tofu_module"),
		moduleLibrary(repo+"//:_rules_tofu_module", "@tf_modules//m", repo+"//:main.tf"),
		fileTarget(repo+"//:main.tf", filepath.Join(ext, "main.tf")))...)
	tofu, _, _ := fakeTofu(t)

	var log strings.Builder
	if err := run(ws, "", tofu, bazel, nil, nil, nil, &log); err != nil {
		t.Fatalf("run: %v\n%s", err, log.String())
	}
	got := lockedProviders(t, filepath.Join(ws, ".terraform.lock.hcl"))
	want := map[string]tofushow.Provider{}
	for _, name := range []string{"null", "http", "random", "time"} {
		want[name] = tofushow.Provider{Source: hashicorp(name)}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("locked against %v, want %v", got, want)
	}
}

// A target whose files OpenTofu cannot load on their own fails pin as it fails
// the build, naming the target, and leaves the lock alone.
func TestRun_TargetCannotLoad(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "app/main.tf"), nullFloor+`module "m" { source = "./m" }`)
	writeFile(t, filepath.Join(ws, "app/m_override.tf"), `module "m" { source = "./other" }`)
	writeFile(t, filepath.Join(ws, ".terraform.lock.hcl"), "# old\n")
	bazel := fakeBazelOutput(t, append(library(ws, "//app:lib", "//app:main.tf"),
		append(library(ws, "//app:deploy", "//app:m_override.tf"), fileTarget("//app:main.tf", filepath.Join(ws, "app/main.tf")))...)...)
	tofu, logPath, _ := fakeTofu(t)

	var log strings.Builder
	err := run(ws, "", tofu, bazel, nil, nil, nil, &log)
	if err == nil {
		t.Fatalf("expected run to fail:\n%s", log.String())
	}
	for _, want := range []string{
		`//app:deploy: tofu cannot load the module in "app"`,
		"Missing module call to override",
		"on app/m_override.tf line 1",
		"An override file must be in the same target as the block it overrides.",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
	if lock, _ := os.ReadFile(filepath.Join(ws, ".terraform.lock.hcl")); string(lock) != "# old\n" {
		t.Errorf("a failed pin must leave the lock alone, got %q", lock)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Errorf("tofu providers lock should not have run, got err=%v", err)
	}
}

// When every required provider is one the root module tags, there is nothing
// to lock: pin writes an empty lock (`tofu`'s header, no providers) over the
// old one without running `tofu providers lock`, so a stale entry can't
// conflict with the tag.
func TestRun_AllProvidersTagged(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "lib/main.tf"), `terraform {
  required_providers {
    null = { source = "hashicorp/null", version = "3.2.4" }
  }
}
`)
	lockPath := filepath.Join(ws, ".terraform.lock.hcl")
	writeFile(t, lockPath, "# old\nprovider \"registry.opentofu.org/hashicorp/null\" {\n  version = \"3.2.3\"\n}\n")
	bazel := fakeBazel(t, ws, "//lib:main.tf")
	tofu, logPath, _ := fakeTofu(t)

	var log strings.Builder
	if err := run(ws, "", tofu, bazel, nil, []string{"hashicorp/null"}, nil, &log); err != nil {
		t.Fatalf("run: %v\n%s", err, log.String())
	}
	lock, err := os.ReadFile(lockPath)
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
		t.Errorf("tofu providers lock should not have run, got err=%v", err)
	}
}

// requiring writes a file that requires the given `<local name> = <entry>`
// lines.
func requiring(t *testing.T, path string, entries ...string) string {
	t.Helper()
	writeFile(t, path, "terraform {\n  required_providers {\n    "+strings.Join(entries, "\n    ")+"\n  }\n}\n")
	return path
}

// Providers are combined by source, never by local name (issue #99).
func TestCombine(t *testing.T) {
	const aws, acme, null = "registry.opentofu.org/hashicorp/aws", "registry.opentofu.org/acme/aws", "registry.opentofu.org/hashicorp/null"
	got := combine([]tofushow.Provider{
		{Source: aws, Version: "~> 5.0"},
		{Source: null},
		{Source: aws, Version: ">= 5.40.0"},
		{Source: acme, Version: "1.2.3"},
		{Source: aws},
		{Source: aws, Version: "~> 5.0"},
	})
	want := []tofushow.Provider{{Source: acme, Version: "1.2.3"}, {Source: aws, Version: ">= 5.40.0, ~> 5.0"}, {Source: null}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("combine = %+v, want %+v", got, want)
	}
}

func TestLocalNames(t *testing.T) {
	got := localNames([]tofushow.Provider{
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

// Two targets in one package are read separately: `aws` maps to different
// providers in //mixed:a and //mixed:b, which OpenTofu would reject in one
// module, and both are locked (issue #99). //stacks:dev and //stacks:prod
// constrain the same provider differently while sharing shared.tf, and both
// constraints apply.
func TestRun_TwoTargetsOneDirectory(t *testing.T) {
	ws := t.TempDir()
	requiring(t, filepath.Join(ws, "stacks/dev.tf"), `null = { source = "hashicorp/null", version = "~> 3.2" }`)
	requiring(t, filepath.Join(ws, "stacks/prod.tf"), `null = { source = "hashicorp/null", version = ">= 3.2.3" }`)
	writeFile(t, filepath.Join(ws, "stacks/shared.tf"), `resource "null_resource" "x" {}`)
	requiring(t, filepath.Join(ws, "mixed/a.tf"), `aws = { source = "hashicorp/aws", version = "~> 5.0" }`)
	requiring(t, filepath.Join(ws, "mixed/b.tf"), `aws = { source = "acme/aws", version = "1.2.3" }`)
	var targets []string
	for label, srcs := range map[string][]string{
		"//stacks:dev":  {"//stacks:dev.tf", "//stacks:shared.tf"},
		"//stacks:prod": {"//stacks:prod.tf", "//stacks:shared.tf"},
		"//mixed:a":     {"//mixed:a.tf"},
		"//mixed:b":     {"//mixed:b.tf"},
	} {
		targets = append(targets, library(ws, label, srcs...)...)
	}
	sort.Strings(targets)
	bazel := fakeBazelOutput(t, targets...)
	tofu, _, _ := fakeTofu(t)

	var log strings.Builder
	if err := run(ws, "", tofu, bazel, []string{"linux_amd64"}, nil, nil, &log); err != nil {
		t.Fatalf("run: %v\n%s", err, log.String())
	}
	got := lockedProviders(t, filepath.Join(ws, ".terraform.lock.hcl"))
	want := map[string]tofushow.Provider{
		"aws":   {Source: "registry.opentofu.org/acme/aws", Version: "1.2.3"},
		"aws-2": {Source: "registry.opentofu.org/hashicorp/aws", Version: "~> 5.0"},
		"null":  {Source: nullSource, Version: ">= 3.2.3, ~> 3.2"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("locked against %v, want %v", got, want)
	}

	// The summary printed when tofu can't meet the constraints shows the
	// directory's combined one.
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
	tofu, _, _ := fakeTofu(t)

	var log strings.Builder
	if err := run(ws, "", tofu, bazel, []string{"linux_amd64"}, nil, nil, &log); err != nil {
		t.Fatalf("run: %v\n%s", err, log.String())
	}
	got := lockedProviders(t, filepath.Join(ws, ".terraform.lock.hcl"))
	if want := map[string]tofushow.Provider{"null": {Source: nullSource, Version: "3.2.3, >= 3.0.0"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("locked against %v, want %v", got, want)
	}
	if !strings.Contains(log.String(), "for the providers required in 4 directories\n") {
		t.Errorf("log = %q", log.String())
	}
}

// Targets listing the same files in a directory, such as two targets over one
// filegroup, share a single `tofu show` call.
func TestRun_ReadsEachFileSetOnce(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "a/main.tf"), nullFloor)
	writeFile(t, filepath.Join(ws, "b/main.tf"), nullFloor)
	bazel := fakeBazelOutput(t,
		ruleTarget("_tf_library", "//a:one", "//a:files"),
		ruleTarget("_tf_library", "//a:two", "//a:files"),
		ruleTarget("filegroup", "//a:files", "//a:main.tf"),
		ruleTarget("_tf_library", "//b:lib", "//b:main.tf"),
		fileTarget("//a:main.tf", filepath.Join(ws, "a/main.tf")),
		fileTarget("//b:main.tf", filepath.Join(ws, "b/main.tf")))
	tofu, _, showLog := fakeTofu(t)

	var log strings.Builder
	if err := run(ws, "", tofu, bazel, nil, nil, nil, &log); err != nil {
		t.Fatalf("run: %v\n%s", err, log.String())
	}
	shows, _ := os.ReadFile(showLog)
	if got := strings.Count(string(shows), "\n"); got != 2 {
		t.Errorf("tofu show ran %d times, want 2:\n%s", got, shows)
	}
}

// fakeBazelBuild writes a bazel stand-in whose `query` prints targets and whose
// `build` writes events to --build_event_json_file, logs its arguments to
// buildLog and exits with exitCode.
func fakeBazelBuild(t *testing.T, events string, exitCode int, targets ...string) (path, buildLog string) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, "bazel")
	buildLog = filepath.Join(dir, "build.log")
	writeFile(t, filepath.Join(dir, "events.json"), events)
	writeFile(t, path, `#!/bin/sh
if [ "$1" = query ]; then
  cat <<'EOF'
`+strings.Join(targets, "\n")+`
EOF
  exit 0
fi
[ "$1" = build ] || exit 2
echo "$@" >> "`+buildLog+`"
for a in "$@"; do
  case "$a" in --build_event_json_file=*) cp "`+filepath.Join(dir, "events.json")+`" "${a#--build_event_json_file=}" ;; esac
done
exit `+fmt.Sprint(exitCode)+`
`)
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path, buildLog
}

// selectRule encodes a rule as `bazel query --noproto:flatten_selects` prints
// it for `srcs = plain + select(branches)`.
func selectRule(class, label string, plain []string, branches map[string][]string) string {
	var entries []any
	for _, cond := range slices.Sorted(maps.Keys(branches)) {
		entries = append(entries, map[string]any{"label": cond, "stringListValue": branches[cond]})
	}
	elements := []any{
		map[string]any{"entries": []any{map[string]any{"label": "//conditions:default", "stringListValue": plain}}},
		map[string]any{"entries": entries},
	}
	return marshalRule(map[string]any{"name": label, "ruleClass": class, "attribute": []any{
		map[string]any{"name": "srcs", "type": "SELECTOR_LIST", "selectorList": map[string]any{"type": "LABEL_LIST", "elements": elements}},
	}})
}

// buildEvents is trimmed Build Event Protocol JSON from Bazel 9.1.0 for
// `bazel build --keep_going //gen:gen.tf.json //gen:und //gen:from_tf.tf`,
// with @EXECROOT@ standing in for workspaceInfo.localExecRoot. The first two
// targets built (//gen:und spans two nested namedSetOfFiles, including a
// non-configuration .txt file); //gen:from_tf.tf failed analysis.
const buildEvents = `{"id":{"pattern":{"pattern":["//gen:gen.tf.json","//gen:und","//gen:from_tf.tf"]}},"children":[{"targetConfigured":{"label":"//gen:gen.tf.json"}}],"expanded":{}}
{"id":{"workspace":{}},"workspaceInfo":{"localExecRoot":"@EXECROOT@"}}
{"id":{"targetConfigured":{"label":"//gen:gen.tf.json"}},"children":[{"targetCompleted":{"label":"//gen:gen.tf.json","configuration":{"id":"91e2"}}}],"configured":{"targetKind":"generated file"}}
{"id":{"namedSet":{"id":"2"}},"namedSetOfFiles":{"files":[{"name":"gen/und/extra.tf","uri":"file:///x","pathPrefix":["bazel-out","k8-fastbuild","bin"]}]}}
{"id":{"namedSet":{"id":"0"}},"namedSetOfFiles":{"files":[{"name":"gen/und.txt","uri":"file:///x","pathPrefix":["bazel-out","k8-fastbuild","bin"]}],"fileSets":[{"id":"2"}]}}
{"id":{"targetCompleted":{"label":"//gen:und","configuration":{"id":"91e2"}}},"completed":{"success":true,"outputGroup":[{"name":"default","fileSets":[{"id":"0"}]}]}}
{"id":{"namedSet":{"id":"1"}},"namedSetOfFiles":{"files":[{"name":"gen/gen.tf.json","uri":"file:///x","pathPrefix":["bazel-out","k8-fastbuild","bin"]}]}}
{"id":{"targetCompleted":{"label":"//gen:gen.tf.json","configuration":{"id":"91e2"}}},"completed":{"success":true,"outputGroup":[{"name":"default","fileSets":[{"id":"1"}]}]}}
{"id":{"configuredLabel":{"label":"@@rules_tofu++tf_providers+tf_providers//hashicorp/null:null","configuration":{"id":"91e2"}}},"aborted":{"reason":"ANALYSIS_FAILURE","description":"no such package"}}
{"id":{"targetConfigured":{"label":"//gen:from_tf.tf"}},"children":[],"aborted":{"reason":"ANALYSIS_FAILURE"}}
`

// parseBuildEvents extracts configuration files from default outputs across
// nested file sets, skips non-configuration files and failed targets, and
// rejects malformed JSON.
func TestParseBuildEvents(t *testing.T) {
	got, err := parseBuildEvents([]byte(strings.ReplaceAll(buildEvents, "@EXECROOT@", "/exec")))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]sourceFile{
		"//gen:gen.tf.json": {{Dir: "gen", Path: "/exec/bazel-out/k8-fastbuild/bin/gen/gen.tf.json"}},
		"//gen:und":         {{Dir: "gen/und", Path: "/exec/bazel-out/k8-fastbuild/bin/gen/und/extra.tf"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseBuildEvents = %+v, want %+v", got, want)
	}
	if _, err := parseBuildEvents([]byte("not JSON\n")); err == nil {
		t.Error("expected an error for events that aren't JSON")
	}
}

// pin builds generated configuration in srcs (both declared outputs and rules
// without declared outputs) in one `bazel build`, merges built outputs into
// their target's directory groups, locks their providers, and warns on labels
// that fail to build while locking the rest.
func TestRun_BuildsGeneratedFiles(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "gen/main.tf"), nullFloor)
	exec := t.TempDir()
	bin := filepath.Join(exec, "bazel-out/k8-fastbuild/bin")
	writeFile(t, filepath.Join(bin, "gen/gen.tf.json"), `{"resource": {"random_id": {"x": {"byte_length": 1}}}}`)
	writeFile(t, filepath.Join(bin, "gen/und/extra.tf"), `resource "time_static" "t" {}`)
	writeFile(t, filepath.Join(bin, "gen/und.txt"), "not configuration")
	bazel, buildLog := fakeBazelBuild(t, strings.ReplaceAll(buildEvents, "@EXECROOT@", exec), 1,
		ruleTarget("_tf_library", "//gen:lib", "//gen:main.tf", "//gen:gen.tf.json", "//gen:und", "//gen:from_tf.tf"),
		fileTarget("//gen:main.tf", filepath.Join(ws, "gen/main.tf")),
		generatingRule("genrule", "//gen:gen", "//gen:gen.tf.json"),
		generatingRule("undeclared", "//gen:und"),
		generatingRule("genrule", "//gen:from_tf", "//gen:from_tf.tf"))
	tofu, _, showLog := fakeTofu(t)

	var log strings.Builder
	if err := run(ws, "", tofu, bazel, nil, nil, nil, &log); err != nil {
		t.Fatalf("run: %v\n%s", err, log.String())
	}
	got := lockedProviders(t, filepath.Join(ws, ".terraform.lock.hcl"))
	want := map[string]tofushow.Provider{
		"null":   {Source: nullSource, Version: ">= 3.0.0"},
		"random": {Source: hashicorp("random")},
		"time":   {Source: hashicorp("time")},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("locked against %v, want %v", got, want)
	}
	builds, _ := os.ReadFile(buildLog)
	if !regexp.MustCompile(`^build --keep_going --remote_download_outputs=toplevel --build_event_json_file=\S+ -- //gen:from_tf.tf //gen:gen.tf.json //gen:und\n$`).Match(builds) {
		t.Errorf("bazel build calls = %q", builds)
	}
	// gen.tf.json is read with main.tf, in their directory.
	if shows, _ := os.ReadFile(showLog); strings.Count(string(shows), "\n") != 2 {
		t.Errorf("tofu show ran other than twice:\n%s", shows)
	}
	if !strings.Contains(log.String(), "pin could not build //gen:from_tf.tf") {
		t.Errorf("expected a warning about //gen:from_tf.tf:\n%s", log.String())
	}
	if strings.Contains(log.String(), "//gen:und") || strings.Contains(log.String(), "//gen:gen.tf.json") {
		t.Errorf("warned about a target that built:\n%s", log.String())
	}
}

// A `bazel build` exit code other than 0 or 1 fails pin and leaves the lock
// unchanged.
func TestRun_BuildFailure(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "gen/main.tf"), nullFloor)
	writeFile(t, filepath.Join(ws, ".terraform.lock.hcl"), "# old\n")
	bazel, _ := fakeBazelBuild(t, "", 2,
		ruleTarget("_tf_library", "//gen:lib", "//gen:main.tf", "//gen:gen.tf"),
		fileTarget("//gen:main.tf", filepath.Join(ws, "gen/main.tf")),
		generatingRule("genrule", "//gen:gen", "//gen:gen.tf"))
	tofu, _, _ := fakeTofu(t)
	var log strings.Builder
	if err := run(ws, "", tofu, bazel, nil, nil, nil, &log); err == nil || !strings.Contains(err.Error(), "bazel build") {
		t.Errorf("expected the build's failure to surface, got %v\n%s", err, log.String())
	}
	if lock, _ := os.ReadFile(filepath.Join(ws, ".terraform.lock.hcl")); string(lock) != "# old\n" {
		t.Errorf("a failed pin must leave the lock alone, got %q", lock)
	}
}

// Each select() branch is read separately alongside unconditional files, so
// branches may declare the same names and every branch's providers are locked.
// A combination OpenTofu cannot load fails pin naming the target and chosen
// branch conditions.
func TestRun_SelectBranches(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "net/main.tf"), `resource "null_resource" "x" {}`)
	requiring(t, filepath.Join(ws, "net/aws.tf"), `null = { source = "hashicorp/null", version = "~> 3.2" }`, `aws = { source = "hashicorp/aws" }`)
	requiring(t, filepath.Join(ws, "net/gcp.tf"), `null = { source = "hashicorp/null", version = ">= 3.2.3" }`, `google = { source = "hashicorp/google" }`)
	targets := []string{
		selectRule("_tf_library", "//net:lib", []string{"//net:main.tf"}, map[string][]string{"//cfg:aws": {"//net:aws.tf"}, "//cfg:gcp": {"//net:gcp.tf"}}),
	}
	for _, name := range []string{"main.tf", "aws.tf", "gcp.tf"} {
		targets = append(targets, fileTarget("//net:"+name, filepath.Join(ws, "net", name)))
	}
	bazel := fakeBazelOutput(t, targets...)
	tofu, _, _ := fakeTofu(t)

	var log strings.Builder
	if err := run(ws, "", tofu, bazel, nil, nil, nil, &log); err != nil {
		t.Fatalf("run: %v\n%s", err, log.String())
	}
	got := lockedProviders(t, filepath.Join(ws, ".terraform.lock.hcl"))
	want := map[string]tofushow.Provider{
		"aws":    {Source: hashicorp("aws")},
		"google": {Source: hashicorp("google")},
		"null":   {Source: nullSource, Version: ">= 3.2.3, ~> 3.2"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("locked against %v, want %v", got, want)
	}

	writeFile(t, filepath.Join(ws, "net/gcp.tf"), `resource "null_resource" "x" {}`)
	err := run(ws, "", tofu, bazel, nil, nil, nil, &log)
	if err == nil || !strings.Contains(err.Error(), "//net:lib (select() //cfg:gcp): ") || !strings.Contains(err.Error(), "Duplicate resource") {
		t.Errorf("expected a load failure naming the gcp branch, got %v", err)
	}
}
