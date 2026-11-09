package tfconfig

import (
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
)

// A literal string is accepted in both syntaxes; a template with a variable
// is rejected in both, the JSON one included, which a nil evaluation context
// would have returned as literal text.
func TestLiteralString(t *testing.T) {
	parser := hclparse.NewParser()
	cases := []struct {
		name, src string
		json      bool
		want      string
		ok        bool
	}{
		{"native literal", `v = "a/b"`, false, "a/b", true},
		{"native template", `v = "${path.module}/b"`, false, "", false},
		{"native number", `v = 1`, false, "", false},
		{"json literal", `{"v": "a/b"}`, true, "a/b", true},
		{"json template", `{"v": "${path.module}/b"}`, true, "", false},
		{"json escaped dollar", `{"v": "$${not}/b"}`, true, "${not}/b", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var f *hcl.File
			var diags hcl.Diagnostics
			if tc.json {
				f, diags = parser.ParseJSON([]byte(tc.src), tc.name+".json")
			} else {
				f, diags = parser.ParseHCL([]byte(tc.src), tc.name+".hcl")
			}
			if diags.HasErrors() {
				t.Fatal(diags)
			}
			attrs, diags := f.Body.JustAttributes()
			if diags.HasErrors() {
				t.Fatal(diags)
			}
			got, ok := LiteralString(attrs["v"].Expr)
			if got != tc.want || ok != tc.ok {
				t.Errorf("LiteralString = %q, %v; want %q, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}
