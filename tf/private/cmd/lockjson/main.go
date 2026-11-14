// Command lockjson reads a .terraform.lock.hcl file and prints its provider
// entries as JSON for the tf_providers module extension's from_lock tag.
//
// A module extension cannot run anything Bazel builds, so this tool is
// compiled with rules_go's host Go SDK by the repository rule in
// //tf/private:host_tools.bzl, as Gazelle builds its own helpers. It parses
// the file and checks each entry's address the way tofu does when reading a
// lock in OpenTofu's depsfile package: the address must be a full
// hostname/namespace/name, must not be a built-in or legacy provider and
// must be written in its canonical normalised form. What else an entry must
// contain to be usable is decided by //tf/providers:lock.bzl, where it can be
// unit-tested.
//
// Usage:
//
//	lockjson <.terraform.lock.hcl>
//
// On success it prints:
//
//	{"providers": [{"address": "...", "host": "...", "namespace": "...",
//	                "name": "...", "version": "...", "hashes": ["..."]}, ...]}
//
// The providers slice and each hashes slice encode as [], never null, and
// constraints and unknown HCL attributes or blocks are omitted. It exits 2
// unless given a single argument. On an I/O error, invalid HCL or an invalid
// provider address it writes the error to stderr and exits 1.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclparse"
	regaddr "github.com/opentofu/registry-address/v2"
)

// lockFile is the schema of a dependency lock file, as tofu writes it.
// Unknown attributes and blocks are ignored, so locks written by later tofu
// versions still read.
type lockFile struct {
	Providers []provider `hcl:"provider,block"`
	Remain    hcl.Body   `hcl:",remain"`
}

// provider is a single provider block in .terraform.lock.hcl.
type provider struct {
	Address     string    `hcl:"address,label" json:"address"`
	Version     string    `hcl:"version,optional" json:"version"`
	Constraints string    `hcl:"constraints,optional" json:"-"`
	Hashes      []string  `hcl:"hashes,optional" json:"hashes"`
	Remain      hcl.Body  `hcl:",remain" json:"-"`
	Range       hcl.Range `hcl:",def_range" json:"-"`

	// The parts of Address, as registry-address reads them.
	Host      string `json:"host"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
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

// convert parses src, using filename in messages, and writes its providers to
// w as JSON.
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
		p := &lock.Providers[i]
		if p.Hashes == nil {
			p.Hashes = []string{}
		}
		if diags := p.parseAddress(); diags.HasErrors() {
			return diags
		}
	}
	return json.NewEncoder(w).Encode(map[string][]provider{"providers": lock.Providers})
}

// parseAddress checks p.Address as OpenTofu's lock reader does and fills in
// its parts. The rules and messages match decodeProviderLockFromHCL in
// OpenTofu's internal/depsfile: a lock is written by tofu, so an address it
// would refuse is an error here too.
func (p *provider) parseAddress() hcl.Diagnostics {
	invalid := func(summary, detail string) hcl.Diagnostics {
		return hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  summary,
			Detail:   detail,
			Subject:  p.Range.Ptr(),
		}}
	}
	addr, err := regaddr.ParseProviderSource(p.Address)
	if err != nil || !addr.HasKnownNamespace() {
		return invalid("Invalid provider source address",
			`The provider source address for a provider lock must be a valid, fully-qualified address of the form "hostname/namespace/type".`)
	}
	if addr.IsBuiltIn() {
		return invalid("Invalid provider source address",
			fmt.Sprintf("Cannot lock a version for built-in provider %s. Built-in providers are bundled inside OpenTofu itself, so you can't select a version for them independently of the OpenTofu release you are currently running.", addr))
	}
	if addr.IsLegacy() {
		return invalid("Invalid provider source address",
			fmt.Sprintf("Provider source address %s is a special provider that is not eligible for dependency locking.", addr))
	}
	if canonical := addr.String(); canonical != p.Address {
		return invalid("Non-normalized provider source address",
			fmt.Sprintf("The provider source address for this provider lock must be written as %q, the fully-qualified and normalized form.", canonical))
	}
	p.Host = addr.Hostname.ForDisplay()
	p.Namespace = addr.Namespace
	p.Name = addr.Type
	return nil
}
