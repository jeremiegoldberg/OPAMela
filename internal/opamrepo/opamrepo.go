// Package opamrepo locates packages inside an opam-repository checkout.
package opamrepo

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Package is one versioned package in a repository.
type Package struct {
	Name    string // "posix-base"
	Version string // "2.0.0"
	Dir     string // <root>/packages/posix-base/posix-base.2.0.0
}

// OpamPath is the path of the package's opam file.
func (p Package) OpamPath() string { return filepath.Join(p.Dir, "opam") }

// RelDir is the package directory relative to the repository root.
func (p Package) RelDir() string {
	return filepath.Join("packages", p.Name, p.Name+"."+p.Version)
}

// Sources is the set of repositories a mirror is built from.
//
// Overlays are searched before Base. This is what makes opam-monorepo usable
// through a mirror: dune-universe/opam-overlays redefines a number of packages
// so that they build with dune, and those definitions have to win over the
// official ones. Merging repositories is a thing you can do when you hold the
// directory yourself, and cannot when you only proxy it.
type Sources struct {
	Base     string
	Overlays []string
}

// Roots returns the search path, highest priority first.
func (s Sources) Roots() []string {
	roots := make([]string, 0, len(s.Overlays)+1)
	roots = append(roots, s.Overlays...)
	if s.Base != "" {
		roots = append(roots, s.Base)
	}
	return roots
}

// List returns every versioned package visible across roots.
//
// Roots are searched in order and the first one to provide a given name and
// version wins, so a later root never shadows an earlier one.
//
// The version comes from the directory name, which is where opam itself reads
// it from. Asking an external tool for it, as tempting as it looks, is both
// slower and less accurate: the directory name is the authority.
func List(roots []string) ([]Package, error) {
	var out []Package
	seen := make(map[string]bool)

	for _, root := range roots {
		packages := filepath.Join(root, "packages")
		entries, err := os.ReadDir(packages)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", packages, err)
		}

		for _, nameDir := range entries {
			if !nameDir.IsDir() {
				continue
			}
			name := nameDir.Name()

			versions, err := os.ReadDir(filepath.Join(packages, name))
			if err != nil {
				return nil, err
			}
			for _, versionDir := range versions {
				if !versionDir.IsDir() {
					continue
				}
				base := versionDir.Name()
				version, ok := versionOf(name, base)
				if !ok {
					continue
				}
				if seen[base] {
					continue // provided by a higher priority root
				}
				dir := filepath.Join(packages, name, base)
				if _, err := os.Stat(filepath.Join(dir, "opam")); err != nil {
					continue // a directory without an opam file is not a package
				}
				seen[base] = true
				out = append(out, Package{Name: name, Version: version, Dir: dir})
			}
		}
	}
	return out, nil
}

// versionOf splits "posix-base.2.0.0" into its version part.
func versionOf(name, dirName string) (string, bool) {
	rest, ok := strings.CutPrefix(dirName, name+".")
	if !ok || rest == "" {
		return "", false
	}
	return rest, true
}

// Find resolves one package by name and version, searching roots in order.
func Find(roots []string, name, version string) (Package, error) {
	if err := ValidName(name); err != nil {
		return Package{}, err
	}
	if err := ValidVersion(version); err != nil {
		return Package{}, err
	}
	for _, root := range roots {
		dir := filepath.Join(root, "packages", name, name+"."+version)
		if _, err := os.Stat(filepath.Join(dir, "opam")); err == nil {
			return Package{Name: name, Version: version, Dir: dir}, nil
		}
	}
	return Package{}, fs.ErrNotExist
}

// ValidName rejects anything that could escape the packages directory.
//
// These names arrive from HTTP request paths, so this is the boundary between
// a request and the filesystem. Everything downstream assumes it ran.
func ValidName(s string) error { return validPathElement("package name", s) }

// ValidVersion applies the same rule to a version string.
func ValidVersion(s string) error { return validPathElement("version", s) }

func validPathElement(what, s string) error {
	if s == "" {
		return fmt.Errorf("empty %s", what)
	}
	if len(s) > 128 {
		return fmt.Errorf("%s too long", what)
	}
	if s == "." || s == ".." || strings.Contains(s, "/") || strings.Contains(s, `\`) {
		return fmt.Errorf("invalid %s %q", what, s)
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '-', r == '_', r == '+', r == '~':
		default:
			return fmt.Errorf("invalid character %q in %s %q", r, what, s)
		}
	}
	return nil
}
