// Command lockjson reads a .terraform.lock.hcl and prints its provider
// entries as JSON, for the tf_providers module extension (from_lock).
//
// A module extension can't run anything Bazel builds, so this is compiled
// with rules_go's host Go SDK by the repository rule in
// //tf/providers:lock_reader.bzl, as Gazelle builds its own helpers. It only
// parses: what an entry must contain to be usable is decided by
// //tf/providers:lock.bzl, where it can be unit-tested.
//
// Usage: lockjson <path>. On success it prints
//
//	{"providers": [{"address": "...", "version": "...", "hashes": ["..."]}, ...]}
//
// On a syntax error it prints the diagnostics to stderr and exits 1.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclparse"
)

// lockFile is the schema of a dependency lock file, as tofu writes it.
// Unknown attributes and blocks are ignored, so locks written by later tofu
// versions still read.
type lockFile struct {
	Providers []provider `hcl:"provider,block"`
	Remain    hcl.Body   `hcl:",remain"`
}

type provider struct {
	Address     string   `hcl:"address,label" json:"address"`
	Version     string   `hcl:"version,optional" json:"version"`
	Constraints string   `hcl:"constraints,optional" json:"-"`
	Hashes      []string `hcl:"hashes,optional" json:"hashes"`
	Remain      hcl.Body `hcl:",remain" json:"-"`
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: lockjson <.terraform.lock.hcl>")
		os.Exit(2)
	}
	src, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := convert(src, os.Args[1], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// convert parses src (named filename in messages) and writes its providers
// to w as JSON.
func convert(src []byte, filename string, w io.Writer) error {
	file, diags := hclparse.NewParser().ParseHCL(src, filename)
	if diags.HasErrors() {
		return diags
	}
	var lock lockFile
	if diags := gohcl.DecodeBody(file.Body, nil, &lock); diags.HasErrors() {
		return diags
	}
	if lock.Providers == nil {
		lock.Providers = []provider{}
	}
	for i := range lock.Providers {
		if lock.Providers[i].Hashes == nil {
			lock.Providers[i].Hashes = []string{}
		}
	}
	return json.NewEncoder(w).Encode(map[string][]provider{"providers": lock.Providers})
}
