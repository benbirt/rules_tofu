package tfconfig

import (
	"reflect"
	"strings"
	"testing"
)

// The cases below follow OpenTofu's own (internal/addrs/module_source_test.go
// in OpenTofu 1.13), for the address forms rules_tofu reads.
func TestParseSource(t *testing.T) {
	for _, tc := range []struct {
		input   string
		want    Source
		wantErr string
	}{
		{input: "./child", want: Source{Kind: KindLocal, Path: "./child"}},
		{input: "../sibling", want: Source{Kind: KindLocal, Path: "../sibling"}},
		{input: `..\sibling`, want: Source{Kind: KindLocal, Path: `..\sibling`}},

		{input: "hashicorp/subnets/cidr", want: Source{Kind: KindRegistry, Host: "registry.opentofu.org", Namespace: "hashicorp", Name: "subnets", System: "cidr"}},
		{input: "hashicorp/subnets/cidr//examples/foo", want: Source{Kind: KindRegistry, Host: "registry.opentofu.org", Namespace: "hashicorp", Name: "subnets", System: "cidr", Subdir: "examples/foo"}},
		{input: "example.com/awesomecorp/network/happycloud", want: Source{Kind: KindRegistry, Host: "example.com", Namespace: "awesomecorp", Name: "network", System: "happycloud"}},
		{input: "Example.COM/Acme/Net/aws", want: Source{Kind: KindRegistry, Host: "example.com", Namespace: "Acme", Name: "Net", System: "aws"}},
		{input: "hashicorp/subnets/cidr//../nope", wantErr: `subdirectory path "../nope" leads outside of the module package`},
		// The target system must be lower case, so this isn't a registry
		// address, and go-getter has nothing for it either.
		{input: "Acme/Net/AWS", wantErr: "cannot detect a supported external module source type"},

		{input: "github.com/hashicorp/terraform-cidr-subnets", want: Source{Kind: KindGit, Remote: "https://github.com/hashicorp/terraform-cidr-subnets.git"}},
		{input: "github.com/hashicorp/terraform-cidr-subnets//example/foo?ref=bar", want: Source{Kind: KindGit, Remote: "https://github.com/hashicorp/terraform-cidr-subnets.git", Ref: "bar", Subdir: "example/foo"}},
		{input: "github.com/hashicorp/terraform-cidr-subnets//example/foo?", want: Source{Kind: KindGit, Remote: "https://github.com/hashicorp/terraform-cidr-subnets.git", Subdir: "example/foo"}},
		{input: "github.com/hashicorp/terraform-cidr-subnets//example/foo?ref=bar/baz", want: Source{Kind: KindGit, Remote: "https://github.com/hashicorp/terraform-cidr-subnets.git", Ref: "bar/baz", Subdir: "example/foo"}},
		{input: "github.com/a/b/c//d", want: Source{Kind: KindGit, Remote: "https://github.com/a/b.git", Subdir: "c/d"}},
		{input: "git://example.com/code/baz.git//bleep/bloop", want: Source{Kind: KindGit, Remote: "git://example.com/code/baz.git", Subdir: "bleep/bloop"}},
		{input: "git::https://example.com/code/baz.git//bleep/bloop", want: Source{Kind: KindGit, Remote: "https://example.com/code/baz.git", Subdir: "bleep/bloop"}},
		{input: "git::ssh://git@example.com/code/baz.git", want: Source{Kind: KindGit, Remote: "ssh://git@example.com/code/baz.git"}},
		{input: "git::git@example.com:code/baz.git//bleep/bloop", want: Source{Kind: KindGit, Remote: "ssh://git@example.com/code/baz.git", Subdir: "bleep/bloop"}},
		{input: "git::github.com/acme/infra?ref=v1", want: Source{Kind: KindGit, Remote: "https://github.com/acme/infra.git", Ref: "v1"}},
		{input: "git::https://example.com/code/baz.git//bleep/bloop?otherthing=blah", wantErr: `doesn't support the git option "otherthing"`},

		{input: "https://example.com/module.zip", want: Source{Kind: KindArchive, URL: "https://example.com/module.zip", Type: "zip"}},
		{input: "https://example.com/module.TAR.GZ//sub/", want: Source{Kind: KindArchive, URL: "https://example.com/module.TAR.GZ", Type: "tar.gz", Subdir: "sub"}},
		{input: "https://example.com/module?archive=tgz&id=3", want: Source{Kind: KindArchive, URL: "https://example.com/module?id=3", Type: "tar.gz"}},
		{input: "file:///tmp/module.tar.tbz2", want: Source{Kind: KindArchive, URL: "file:///tmp/module.tar.tbz2", Type: "tar.bz2"}},
		// A `//` in the query is the query's, not a subdirectory.
		{input: "https://example.com/dl.zip?path=a//b", want: Source{Kind: KindArchive, URL: "https://example.com/dl.zip?path=a//b", Type: "zip"}},
		{input: "https://example.com/module", wantErr: "isn't an archive OpenTofu would unpack"},
		{input: "https://example.com/module?archive=tar", wantErr: "isn't an archive OpenTofu would unpack"},

		{input: "gcs::https://www.googleapis.com/storage/v1/BUCKET_NAME/PATH_TO_MODULE", wantErr: "not gcs:: sources"},
		{input: "s3-eu-west-1.amazonaws.com/examplecorp-terraform-modules/vpc.zip", wantErr: "not Bitbucket, S3 or GCS sources"},
		{input: "/tmp/foo//example/../../invalid", wantErr: `subdirectory path "../invalid" leads outside of the module package`},
		{input: "boop/bloop", wantErr: "OpenTofu cannot detect a supported external module source type for boop/bloop"},
	} {
		got, err := ParseSource(tc.input)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("ParseSource(%q) = %+v, %v; want an error containing %q", tc.input, got, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseSource(%q): %v", tc.input, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParseSource(%q) = %+v, want %+v", tc.input, got, tc.want)
		}
	}
}

func TestSplitSubdir(t *testing.T) {
	for in, want := range map[string][2]string{
		"acme/net/aws":                           {"acme/net/aws", ""},
		"acme/net/aws//modules/x":                {"acme/net/aws", "modules/x"},
		"git::https://e.com/x.git//vpc?ref=v1.0": {"git::https://e.com/x.git?ref=v1.0", "vpc"},
		"https://e.com/x.tar.gz":                 {"https://e.com/x.tar.gz", ""},
		// Neither `://` nor `//` counts inside a query, as in go-getter's
		// SourceDirSubdir (issue #102).
		"https://e.com/dl.zip?path=a//b":                  {"https://e.com/dl.zip?path=a//b", ""},
		"https://e.com/a.zip//sub?next=http://y//z":       {"https://e.com/a.zip?next=http://y//z", "sub"},
		"github.com/acme/net//vpc?ref=feature://x":        {"github.com/acme/net?ref=feature://x", "vpc"},
		"github.com/acme/net?ref=a//b":                    {"github.com/acme/net?ref=a//b", ""},
		"git::ssh://git@e.com/x.git//modules/a?ref=v1//2": {"git::ssh://git@e.com/x.git?ref=v1//2", "modules/a"},
		"file:///srv/modules/x//sub":                      {"file:///srv/modules/x", "sub"},
	} {
		base, subdir := SplitSubdir(in)
		if base != want[0] || subdir != want[1] {
			t.Errorf("SplitSubdir(%q) = (%q, %q), want %q", in, base, subdir, want)
		}
	}
}

func TestMatch(t *testing.T) {
	for _, tc := range []struct {
		provider, source, subdir string
		ok                       bool
	}{
		{"acme/net/aws", "ACME/Net/aws", "", true},
		{"acme/net/aws", "registry.opentofu.org/acme/net/aws", "", true},
		{"acme/net/aws", "acme/net/aws//modules/x", "modules/x", true},
		{"acme/net/aws//modules", "acme/net/aws//modules/x", "x", true},
		{"acme/net/aws//modules/x", "acme/net/aws//modules", "", false},
		{"acme/net/aws//mod", "acme/net/aws//modules", "", false},
		{"acme/net/aws", "example.com/acme/net/aws", "", false},
		{"git::https://e.com/r.git//vpc?ref=v1", "git::https://e.com/r.git//vpc/sub?ref=v1", "sub", true},
		{"git::https://e.com/r.git?ref=v1", "git::https://e.com/r.git//vpc?ref=v1", "vpc", true},
		{"git::https://e.com/r.git?ref=v1", "git::https://e.com/r.git?ref=v2", "", false},
		// The shorthand and the URL it stands for are one package.
		{"github.com/acme/infra?ref=v1", "git::https://github.com/acme/infra.git//vpc?ref=v1", "vpc", true},
		{"acme/net/aws", "acme/other/aws", "", false},
		{"./local", "./local", "", false},
	} {
		subdir, ok := Match(tc.provider, tc.source)
		if subdir != tc.subdir || ok != tc.ok {
			t.Errorf("Match(%q, %q) = (%q, %v), want (%q, %v)", tc.provider, tc.source, subdir, ok, tc.subdir, tc.ok)
		}
	}
}

func TestBestMatch(t *testing.T) {
	for _, tc := range []struct {
		name      string
		providers []string
		source    string
		want      string // the provider picked, "" for none
		subdir    string
	}{
		{
			// Issue #97: the parent's address is the longer string.
			name:      "a submodule beats its parent written with the host",
			providers: []string{"registry.opentofu.org/acme/net/aws", "acme/net/aws//modules"},
			source:    "acme/net/aws//modules/x",
			want:      "acme/net/aws//modules",
			subdir:    "x",
		},
		{
			name:      "whatever the order",
			providers: []string{"acme/net/aws//modules", "registry.opentofu.org/acme/net/aws"},
			source:    "acme/net/aws//modules/x",
			want:      "acme/net/aws//modules",
			subdir:    "x",
		},
		{
			name:      "the deepest of three",
			providers: []string{"acme/net/aws//modules", "acme/net/aws//modules/x/y", "acme/net/aws"},
			source:    "acme/net/aws//modules/x/y/z",
			want:      "acme/net/aws//modules/x/y",
			subdir:    "z",
		},
		{
			name:      "a git shorthand parent and the URL of its submodule",
			providers: []string{"git::https://github.com/o/r.git//modules", "github.com/o/r"},
			source:    "github.com/o/r//modules/x",
			want:      "git::https://github.com/o/r.git//modules",
			subdir:    "x",
		},
		{
			name:      "the parent when the submodule is not the one called",
			providers: []string{"acme/net/aws//modules/x", "registry.opentofu.org/acme/net/aws"},
			source:    "acme/net/aws//modules/y",
			want:      "registry.opentofu.org/acme/net/aws",
			subdir:    "modules/y",
		},
		{
			name:      "the first of two spellings of one address",
			providers: []string{"registry.opentofu.org/acme/net/aws", "acme/net/aws"},
			source:    "acme/net/aws//modules/x",
			want:      "registry.opentofu.org/acme/net/aws",
			subdir:    "modules/x",
		},
		{
			name:      "none",
			providers: []string{"acme/dns/aws", "./local", ""},
			source:    "acme/net/aws",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i, subdir := BestMatch(tc.providers, tc.source)
			got := ""
			if i >= 0 {
				got = tc.providers[i]
			}
			if got != tc.want || subdir != tc.subdir || (tc.want == "") != (i == -1) {
				t.Errorf("BestMatch(%q, %q) = (%d %q, %q), want (%q, %q)", tc.providers, tc.source, i, got, subdir, tc.want, tc.subdir)
			}
		})
	}
}

func TestNormalizeProvider(t *testing.T) {
	for in, want := range map[string]string{
		"hashicorp/aws":                       "registry.opentofu.org/hashicorp/aws",
		"PostHog/posthog":                     "registry.opentofu.org/posthog/posthog",
		"Registry.OpenTofu.org/HashiCorp/AWS": "registry.opentofu.org/hashicorp/aws",
		"example.com/acme/other":              "example.com/acme/other",
	} {
		got, err := NormalizeProvider(in)
		if err != nil || got != want {
			t.Errorf("NormalizeProvider(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := NormalizeProvider("not a/valid/provider/address"); err == nil {
		t.Error("expected an error for an invalid address")
	}
}
