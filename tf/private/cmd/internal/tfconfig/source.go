package tfconfig

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strings"

	regaddr "github.com/opentofu/registry-address/v2"
)

// The kinds of module source rules_tofu understands.
const (
	KindLocal    = "local"
	KindRegistry = "registry"
	KindGit      = "git"
	KindArchive  = "archive"
)

// Source is a module `source` address, read as OpenTofu reads it
// (addrs.ParseModuleSource in OpenTofu):
//
//   - a local path, starting `./` or `../`;
//   - else a module registry address, if OpenTofu's own registry-address
//     library can parse it as one;
//   - else a remote address, which OpenTofu hands to go-getter's detectors.
//     rules_tofu reads only the remote forms it can fetch: git (`git::<url>`,
//     `git://`, `github.com/<owner>/<repo>`, `git@<host>:<path>`) and
//     archives over http(s) or from a file:// URL.
type Source struct {
	Kind string `json:"kind"`

	// For a registry address. Host is always set, the default registry's if
	// the address leaves it out.
	Host      string `json:"host,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name,omitempty"`
	System    string `json:"system,omitempty"`

	// For git: the repository to fetch, and `?ref=` if given.
	Remote string `json:"remote,omitempty"`
	Ref    string `json:"ref,omitempty"`

	// For an archive: its URL, and its type as Bazel's download_and_extract
	// names it.
	URL  string `json:"url,omitempty"`
	Type string `json:"type,omitempty"`

	// The directory within the package (`//<subdir>`), cleaned.
	Subdir string `json:"subdir,omitempty"`

	// For a local path, the path itself.
	Path string `json:"path,omitempty"`
}

// IsLocal reports whether tofu reads source as a local path.
func IsLocal(source string) bool {
	return strings.HasPrefix(source, "./") || strings.HasPrefix(source, "../") ||
		strings.HasPrefix(source, `.\`) || strings.HasPrefix(source, `..\`)
}

// ParseSource reads a module source address. It fails on addresses OpenTofu
// would reject, and on remote forms rules_tofu can't fetch.
func ParseSource(raw string) (Source, error) {
	if IsLocal(raw) {
		return Source{Kind: KindLocal, Path: raw}, nil
	}
	if m, err := regaddr.ParseModuleSource(raw); err == nil {
		return Source{
			Kind:      KindRegistry,
			Host:      m.Package.Host.String(),
			Namespace: m.Package.Namespace,
			Name:      m.Package.Name,
			System:    m.Package.TargetSystem,
			Subdir:    m.Subdir,
		}, nil
	}
	return parseRemote(raw)
}

// Package is the identity of the package a source's module is in, without
// its subdirectory, in a form two sources can be compared by: registry
// packages as `<host>/<namespace>/<name>/<system>` in lower case, as the
// registry treats them; git as the repository and ref; archives as their URL.
func (s Source) Package() string {
	switch s.Kind {
	case KindRegistry:
		return strings.ToLower(strings.Join([]string{s.Host, s.Namespace, s.Name, s.System}, "/"))
	case KindGit:
		return "git::" + s.Remote + "?ref=" + s.Ref
	case KindArchive:
		return s.URL + "#" + s.Type
	}
	return s.Path
}

// IsRegistry reports whether tofu reads source as a module registry address.
func IsRegistry(source string) bool {
	s, err := ParseSource(source)
	return err == nil && s.Kind == KindRegistry
}

// NormalizeSource is the form in which two sources are compared: the
// package (see Source.Package) and its subdirectory. A source that doesn't
// parse is compared as written.
func NormalizeSource(source string) string {
	s, err := ParseSource(source)
	if err != nil {
		return source
	}
	if s.Subdir != "" {
		return s.Package() + "//" + s.Subdir
	}
	return s.Package()
}

// Match reports whether a module whose address is provider supplies a call
// to source, and in which subdirectory of provider's directory: source must
// be in the same package, at provider's subdirectory or below it.
func Match(provider, source string) (subdir string, ok bool) {
	p, err := ParseSource(provider)
	if err != nil || p.Kind == KindLocal {
		return "", false
	}
	s, err := ParseSource(source)
	if err != nil || s.Kind == KindLocal || p.Package() != s.Package() {
		return "", false
	}
	switch {
	case p.Subdir == s.Subdir:
		return "", true
	case p.Subdir == "":
		return s.Subdir, true
	case strings.HasPrefix(s.Subdir, p.Subdir+"/"):
		return s.Subdir[len(p.Subdir)+1:], true
	}
	return "", false
}

// SplitSubdir splits off a source's `//<subdir>` as go-getter does: at the
// first `//` that isn't part of a `scheme://`. A query string (`?ref=`)
// stays with the base address.
func SplitSubdir(source string) (string, string) {
	rest := source
	offset := 0
	if i := strings.Index(rest, "://"); i >= 0 {
		offset = i + 3
		rest = rest[offset:]
	}
	i := strings.Index(rest, "//")
	if i < 0 {
		return source, ""
	}
	base := source[:offset+i]
	subdir := rest[i+2:]
	if q := strings.Index(subdir, "?"); q >= 0 {
		base += subdir[q:]
		subdir = subdir[:q]
	}
	return base, subdir
}

// archiveTypes maps the archive extensions (and `?archive=` values) that
// OpenTofu unpacks as a module package to the types Bazel's
// download_and_extract names them by.
var archiveTypes = map[string]string{
	"zip":      "zip",
	"tar.bz2":  "tar.bz2",
	"tar.tbz2": "tar.bz2",
	"tar.gz":   "tar.gz",
	"tgz":      "tar.gz",
	"tar.xz":   "tar.xz",
	"txz":      "tar.xz",
}

func parseRemote(raw string) (Source, error) {
	base, subdir := SplitSubdir(raw)
	subdir, err := cleanSubdir(subdir)
	if err != nil {
		return Source{}, err
	}

	forced, addr, ok := strings.Cut(base, "::")
	if !ok || strings.Contains(forced, "/") {
		forced, addr = "", base
	}
	switch forced {
	case "":
	case "git":
		return parseGit(addr, subdir, raw)
	default:
		return Source{}, fmt.Errorf("rules_tofu can't fetch %q: it fetches registry modules, git repositories and archives, not %s:: sources", raw, forced)
	}

	switch {
	case strings.HasPrefix(addr, "github.com/"), strings.HasPrefix(addr, "git@"), strings.HasPrefix(addr, "git://"):
		return parseGit(addr, subdir, raw)
	case strings.HasPrefix(addr, "https://"), strings.HasPrefix(addr, "http://"), strings.HasPrefix(addr, "file://"):
		return parseArchive(addr, subdir, raw)
	case strings.HasPrefix(addr, "bitbucket.org/"), strings.Contains(addr, "amazonaws.com/"), strings.Contains(addr, "googleapis.com/"):
		return Source{}, fmt.Errorf("rules_tofu can't fetch %q: it fetches registry modules, git repositories and archives, not Bitbucket, S3 or GCS sources", raw)
	case strings.HasPrefix(addr, "/"):
		return Source{}, fmt.Errorf("rules_tofu can't use %q: a module outside the workspace must come from a tf_library", raw)
	}
	// OpenTofu's own message, for the same case.
	return Source{}, fmt.Errorf("OpenTofu cannot detect a supported external module source type for %s", raw)
}

// cleanSubdir cleans a `//<subdir>`, rejecting one that leads out of its
// package, as OpenTofu does.
func cleanSubdir(subdir string) (string, error) {
	if subdir == "" {
		return "", nil
	}
	clean := path.Clean(subdir)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("subdirectory path %q leads outside of the module package", clean)
	}
	if clean == "." {
		return "", nil
	}
	return strings.Trim(clean, "/"), nil
}

// parseGit reads a git source, normalising go-getter's shorthands the way its
// GitHub and git detectors do: `github.com/<owner>/<repo>[/<subdir>]` is
// https://github.com/<owner>/<repo>.git, and `git@<host>:<path>` is
// ssh://git@<host>/<path>.
func parseGit(addr, subdir, raw string) (Source, error) {
	addr, query, _ := strings.Cut(addr, "?")
	params, err := url.ParseQuery(query)
	if err != nil {
		return Source{}, fmt.Errorf("%q: %w", raw, err)
	}
	var unknown []string
	for key := range params {
		if key != "ref" && key != "depth" {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return Source{}, fmt.Errorf("rules_tofu can't fetch %q: it doesn't support the git option %q", raw, unknown[0])
	}

	switch {
	case strings.HasPrefix(addr, "github.com/"):
		parts := strings.SplitN(strings.TrimPrefix(addr, "github.com/"), "/", 3)
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			return Source{}, errors.New("GitHub URLs should be github.com/username/repo")
		}
		addr = "https://github.com/" + parts[0] + "/" + strings.TrimSuffix(parts[1], ".git") + ".git"
		if len(parts) == 3 {
			// The detector's subdir comes first: the given one is inside it.
			if subdir, err = cleanSubdir(path.Join(parts[2], subdir)); err != nil {
				return Source{}, err
			}
		}
	case strings.HasPrefix(addr, "git@"):
		host, repoPath, ok := strings.Cut(strings.TrimPrefix(addr, "git@"), ":")
		if !ok || host == "" || repoPath == "" {
			return Source{}, fmt.Errorf("%q: expected git@<host>:<path>", raw)
		}
		addr = "ssh://git@" + host + "/" + strings.TrimPrefix(repoPath, "/")
	}
	if addr == "" {
		return Source{}, fmt.Errorf("%q: no repository URL", raw)
	}
	return Source{Kind: KindGit, Remote: addr, Ref: params.Get("ref"), Subdir: subdir}, nil
}

// parseArchive reads an archive's URL. Its type is `?archive=`, else its
// extension; anything else is an http URL that OpenTofu would ask for an
// X-Terraform-Get redirect, which rules_tofu doesn't follow.
func parseArchive(addr, subdir, raw string) (Source, error) {
	u, err := url.Parse(addr)
	if err != nil {
		return Source{}, fmt.Errorf("%q: %w", raw, err)
	}
	query := u.Query()
	kind := query.Get("archive")
	if kind != "" {
		query.Del("archive")
		u.RawQuery = query.Encode()
	} else {
		name := strings.ToLower(path.Base(u.Path))
		for ext := range archiveTypes {
			if strings.HasSuffix(name, "."+ext) && len(ext) > len(kind) {
				kind = ext
			}
		}
	}
	bazelType, ok := archiveTypes[kind]
	if !ok {
		var exts []string
		for ext := range archiveTypes {
			exts = append(exts, "."+ext)
		}
		sort.Strings(exts)
		return Source{}, fmt.Errorf("rules_tofu can't fetch %q: it isn't an archive OpenTofu would unpack; name one ending in %s, or add ?archive=<type>",
			raw, strings.Join(exts, ", "))
	}
	return Source{Kind: KindArchive, URL: u.String(), Type: bazelType, Subdir: subdir}, nil
}

// DefaultRegistryHost is the module registry an address may leave out.
var DefaultRegistryHost = regaddr.DefaultModuleRegistryHost.String()
