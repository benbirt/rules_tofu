package tfconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
)

// testExts are the extensions of the files `tofu test` reads. Since OpenTofu
// 1.8, a .tofutest.hcl or .tofutest.json file shadows the .tftest.hcl or
// .tftest.json file of the same name, as .tofu shadows .tf.
var testExts = []string{".tofutest.hcl", ".tftest.hcl", ".tofutest.json", ".tftest.json"}

// testExt returns name's test file extension, or "" if it isn't a test file.
func testExt(name string) string {
	for _, ext := range testExts {
		if strings.HasSuffix(name, ext) {
			return ext
		}
	}
	return ""
}

// IsTestFile reports whether name is a file `tofu test` reads: a
// `.tftest.hcl`, `.tofutest.hcl`, `.tftest.json` or `.tofutest.json` file.
func IsTestFile(name string) bool {
	return testExt(name) != ""
}

// EffectiveTests drops the files of files that tofu doesn't read: each
// `.tftest.hcl` or `.tftest.json` file that a `.tofutest.hcl` or
// `.tofutest.json` file of the same path shadows, and each whose name starts
// with a dot (see ignored). The order of the rest is kept.
func EffectiveTests(files []string) []string {
	present := map[string]bool{}
	for _, f := range files {
		present[f] = true
	}
	var kept []string
	for _, f := range files {
		if ignored(filepath.Base(f)) {
			continue
		}
		ext := testExt(f)
		if ext == ".tftest.hcl" || ext == ".tftest.json" {
			tofu := strings.TrimSuffix(f, ext) + strings.Replace(ext, ".tftest", ".tofutest", 1)
			if present[tofu] {
				continue
			}
		}
		kept = append(kept, f)
	}
	return kept
}

// TestFiles lists the test files `tofu test` reads in dir, sorted by name:
// those with a test extension, less the ones EffectiveTests drops. A
// directory that doesn't exist holds none.
func TestFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && IsTestFile(e.Name()) {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	return EffectiveTests(files), nil
}

// TestRunKey is the key under which `tofu init` records the module a `run`
// block loads in .terraform/modules/modules.json: "test.", then the test
// file's path relative to the root module with "/" as ".", then the run's
// name. Only a .tftest.hcl extension is dropped, as tofu's buildTestModules
// in internal/configs/config_build.go drops only that one; the other test
// extensions stay in the key. tests/helper.tftest.hcl with `run "setup"`
// gives test.tests.helper.setup; tests/helper.tofutest.hcl gives
// test.tests.helper.tofutest.hcl.setup.
func TestRunKey(relFile, run string) string {
	stem := strings.TrimSuffix(filepath.ToSlash(relFile), ".tftest.hcl")
	return "test." + strings.ReplaceAll(stem, "/", ".") + "." + run
}

var (
	runSchema       = &hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: "run", LabelNames: []string{"name"}}}}
	runModuleSchema = &hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: "module"}}}
)

// ReadTestCalls parses the `module` block of each `run` block of files, test
// files that `tofu test` reads (see EffectiveTests). Each is returned as a
// Call named after its run, whose Source is the module the run loads in
// place of the configuration under test and whose Range locates the source
// attribute, sorted by file name and byte offset. Two runs of one name in a
// file, or a `module` block without a literal source, return an error.
func ReadTestCalls(files []string) ([]Call, error) {
	parser := hclparse.NewParser()
	var calls []Call
	for _, file := range EffectiveTests(files) {
		f, err := parse(parser, file)
		if err != nil {
			return nil, err
		}
		content, _, diags := f.Body.PartialContent(runSchema)
		if diags.HasErrors() {
			return nil, diags
		}
		seen := map[string]bool{}
		for _, run := range content.Blocks {
			name := run.Labels[0]
			if seen[name] {
				return nil, fmt.Errorf("%s: duplicate run %q", run.DefRange, name)
			}
			seen[name] = true
			inner, _, diags := run.Body.PartialContent(runModuleSchema)
			if diags.HasErrors() {
				return nil, diags
			}
			for _, module := range inner.Blocks {
				attrs, _, diags := module.Body.PartialContent(sourceSchema)
				if diags.HasErrors() {
					return nil, diags
				}
				attr, ok := attrs.Attributes["source"]
				if !ok {
					return nil, fmt.Errorf("%s: run %q: the module block has no source", module.DefRange, name)
				}
				source, ok := LiteralString(attr.Expr)
				if !ok {
					return nil, fmt.Errorf("%s: run %q: rules_tofu needs the module `source` to be a literal string", attr.Range, name)
				}
				calls = append(calls, Call{Name: name, Source: source, Range: attr.Range})
			}
		}
	}
	sort.SliceStable(calls, func(i, j int) bool {
		if calls[i].Range.Filename != calls[j].Range.Filename {
			return calls[i].Range.Filename < calls[j].Range.Filename
		}
		return calls[i].Range.Start.Byte < calls[j].Range.Start.Byte
	})
	return calls, nil
}
