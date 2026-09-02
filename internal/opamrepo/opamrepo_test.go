package opamrepo

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func write(t *testing.T, root, name, version string) {
	t.Helper()
	dir := filepath.Join(root, "packages", name, name+"."+version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "opam"), []byte(`opam-version: "2.0"`), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestListReadsVersionFromDirectoryName(t *testing.T) {
	root := t.TempDir()
	write(t, root, "dune", "3.0.0")
	write(t, root, "dune", "3.1.1")
	write(t, root, "posix-base", "2.0.0")
	// A directory that is not a package must be ignored, not guessed at.
	if err := os.MkdirAll(filepath.Join(root, "packages", "dune", "not-a-version"), 0o755); err != nil {
		t.Fatal(err)
	}

	pkgs, err := List([]string{root})
	if err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, p := range pkgs {
		got = append(got, p.Name+" "+p.Version)
	}
	sort.Strings(got)
	want := []string{"dune 3.0.0", "dune 3.1.1", "posix-base 2.0.0"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got %v, want %v", got, want)
			break
		}
	}
}

func TestListDeduplicatesAcrossRootsInPriorityOrder(t *testing.T) {
	overlay, base := t.TempDir(), t.TempDir()
	write(t, base, "foo", "1.0")
	write(t, overlay, "foo", "1.0")
	write(t, base, "bar", "1.0")

	pkgs, err := List(Sources{Base: base, Overlays: []string{overlay}}.Roots())
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 2 {
		t.Fatalf("got %d packages, want 2", len(pkgs))
	}
	for _, p := range pkgs {
		if p.Name != "foo" {
			continue
		}
		if want := filepath.Join(overlay, "packages", "foo", "foo.1.0"); p.Dir != want {
			t.Errorf("foo resolved to %s, want the overlay at %s", p.Dir, want)
		}
	}
}

func TestFindSearchesRootsInOrder(t *testing.T) {
	overlay, base := t.TempDir(), t.TempDir()
	write(t, base, "foo", "1.0")
	write(t, overlay, "foo", "1.0")
	roots := Sources{Base: base, Overlays: []string{overlay}}.Roots()

	pkg, err := Find(roots, "foo", "1.0")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(overlay, "packages", "foo", "foo.1.0"); pkg.Dir != want {
		t.Errorf("Dir = %s, want %s", pkg.Dir, want)
	}

	if _, err := Find(roots, "foo", "9.9"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want fs.ErrNotExist", err)
	}
}

// Find receives values straight out of an HTTP request path. This is the check
// that keeps them from becoming filesystem paths.
func TestFindRejectsHostileNames(t *testing.T) {
	root := t.TempDir()
	write(t, root, "foo", "1.0")

	for _, tc := range [][2]string{
		{"../../etc", "1.0"},
		{"foo/../../etc", "1.0"},
		{"foo", "../../../etc/passwd"},
		{"..", "1.0"},
		{".", "1.0"},
		{"", "1.0"},
		{"foo", ""},
		{`foo\bar`, "1.0"},
		{"foo\x00bar", "1.0"},
		{"foo bar", "1.0"},
	} {
		if _, err := Find([]string{root}, tc[0], tc[1]); err == nil {
			t.Errorf("Find(%q, %q) = nil error, want rejection", tc[0], tc[1])
		}
	}
}

func TestValidNameAcceptsRealOpamNames(t *testing.T) {
	// Names and versions taken from opam-repository.
	for _, n := range []string{"dune", "posix-base", "ppx_deriving", "conf-gmp", "0install", "cohttp-lwt-unix"} {
		if err := ValidName(n); err != nil {
			t.Errorf("ValidName(%q) = %v", n, err)
		}
	}
	for _, v := range []string{"3.0.0", "1.0", "0.14.1+dune", "2.0.0~beta1", "1.2.3-1", "v0.15.0"} {
		if err := ValidVersion(v); err != nil {
			t.Errorf("ValidVersion(%q) = %v", v, err)
		}
	}
}

func TestRelDir(t *testing.T) {
	p := Package{Name: "posix-base", Version: "2.0.0"}
	if got, want := p.RelDir(), filepath.Join("packages", "posix-base", "posix-base.2.0.0"); got != want {
		t.Errorf("RelDir() = %q, want %q", got, want)
	}
}
