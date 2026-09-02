package mirror

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jeremiegoldberg/opamela/internal/opamfile"
	"github.com/jeremiegoldberg/opamela/internal/opamrepo"
)

// writePackage lays out packages/<name>/<name>.<version>/opam under root.
func writePackage(t *testing.T, root, name, version, content string) {
	t.Helper()
	dir := filepath.Join(root, "packages", name, name+"."+version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "opam"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func opamWithSrc(src string) string {
	return `opam-version: "2.0"
synopsis: "test package"
url {
  src: "` + src + `"
  checksum: "sha256=0000000000000000000000000000000000000000000000000000000000000000"
}
`
}

func build(t *testing.T, src opamrepo.Sources, base string) (string, Stats) {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "mirror")
	b := &Builder{BaseURL: base, Workers: 2}
	stats, err := b.Build(context.Background(), src, dst)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return dst, stats
}

// The prototype this replaces wrote the rewritten file to
// packages/<name>/opam, one level above the versioned directory. opam never
// reads that path, so the mirror redirected precisely nothing while looking
// like it worked. This test is the reason it cannot happen again.
func TestBuildWritesToVersionedDirectory(t *testing.T) {
	up := t.TempDir()
	writePackage(t, up, "foo", "1.0", opamWithSrc("https://upstream.example/foo-1.0.tar.gz"))

	dst, stats := build(t, opamrepo.Sources{Base: up}, "https://mirror.internal")

	versioned := filepath.Join(dst, "packages", "foo", "foo.1.0", "opam")
	if _, err := os.Stat(versioned); err != nil {
		t.Fatalf("rewritten file missing at %s: %v", versioned, err)
	}

	stray := filepath.Join(dst, "packages", "foo", "opam")
	if _, err := os.Stat(stray); err == nil {
		t.Errorf("wrote a stray opam file at %s, which opam never reads", stray)
	}

	data, err := os.ReadFile(versioned)
	if err != nil {
		t.Fatal(err)
	}
	u, err := opamfile.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	want := "https://mirror.internal/download/foo/1.0/foo-1.0.tar.gz"
	if u.Src != want {
		t.Errorf("src = %q, want %q", u.Src, want)
	}
	if stats.Rewritten != 1 {
		t.Errorf("Rewritten = %d, want 1", stats.Rewritten)
	}
}

func TestBuildLeavesUnmirrorableSourcesAlone(t *testing.T) {
	up := t.TempDir()
	writePackage(t, up, "gitpkg", "1.0", opamWithSrc("git+https://example.org/thing.git"))
	writePackage(t, up, "ftppkg", "1.0", opamWithSrc("ftp://example.org/thing.tar.gz"))

	dst, stats := build(t, opamrepo.Sources{Base: up}, "https://mirror.internal")

	if stats.Passthrough != 2 {
		t.Errorf("Passthrough = %d, want 2", stats.Passthrough)
	}
	for _, name := range []string{"gitpkg", "ftppkg"} {
		data, err := os.ReadFile(filepath.Join(dst, "packages", name, name+".1.0", "opam"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "mirror.internal") {
			t.Errorf("%s: rewrote a source that cannot be served as a file", name)
		}
	}
}

func TestBuildCountsSourcelessPackages(t *testing.T) {
	up := t.TempDir()
	writePackage(t, up, "conf-thing", "1", `opam-version: "2.0"
synopsis: "a system dependency"
depends: ["conf-pkg-config" {build}]
`)

	dst, stats := build(t, opamrepo.Sources{Base: up}, "https://mirror.internal")

	if stats.Sourceless != 1 {
		t.Errorf("Sourceless = %d, want 1", stats.Sourceless)
	}
	// A sourceless package is still part of the repository.
	if _, err := os.Stat(filepath.Join(dst, "packages", "conf-thing", "conf-thing.1", "opam")); err != nil {
		t.Errorf("sourceless package not carried over: %v", err)
	}
}

func TestBuildCarriesExtraFiles(t *testing.T) {
	up := t.TempDir()
	writePackage(t, up, "foo", "1.0", opamWithSrc("https://upstream.example/foo-1.0.tar.gz"))
	patches := filepath.Join(up, "packages", "foo", "foo.1.0", "files")
	if err := os.MkdirAll(patches, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(patches, "fix.patch"), []byte("--- a\n+++ b\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dst, _ := build(t, opamrepo.Sources{Base: up}, "https://mirror.internal")

	got, err := os.ReadFile(filepath.Join(dst, "packages", "foo", "foo.1.0", "files", "fix.patch"))
	if err != nil {
		t.Fatalf("patch not carried over: %v", err)
	}
	if string(got) != "--- a\n+++ b\n" {
		t.Errorf("patch altered: %q", got)
	}
}

func TestOverlayTakesPrecedence(t *testing.T) {
	base := t.TempDir()
	overlay := t.TempDir()
	writePackage(t, base, "foo", "1.0", opamWithSrc("https://upstream.example/official.tar.gz"))
	writePackage(t, overlay, "foo", "1.0", opamWithSrc("https://upstream.example/dune-flavoured.tar.gz"))

	dst, stats := build(t, opamrepo.Sources{Base: base, Overlays: []string{overlay}}, "https://mirror.internal")

	if stats.Packages != 1 {
		t.Errorf("Packages = %d, want 1 after deduplication", stats.Packages)
	}
	data, err := os.ReadFile(filepath.Join(dst, "packages", "foo", "foo.1.0", "opam"))
	if err != nil {
		t.Fatal(err)
	}
	// The rewritten URL is derived from the overlay's source archive name.
	if !strings.Contains(string(data), "/download/foo/1.0/dune-flavoured.tar.gz") {
		t.Errorf("overlay did not win: %s", data)
	}
}

func TestIndexIsDeterministicAndComplete(t *testing.T) {
	up := t.TempDir()
	writePackage(t, up, "foo", "1.0", opamWithSrc("https://upstream.example/foo-1.0.tar.gz"))
	writePackage(t, up, "bar", "2.0", opamWithSrc("https://upstream.example/bar-2.0.tar.gz"))

	first, _ := build(t, opamrepo.Sources{Base: up}, "https://mirror.internal")
	second, _ := build(t, opamrepo.Sources{Base: up}, "https://mirror.internal")

	a, err := os.ReadFile(filepath.Join(first, IndexName))
	if err != nil {
		t.Fatalf("no index generated: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(second, IndexName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Error("two builds of the same input produced different index tarballs")
	}

	names := tarNames(t, a)
	for _, want := range []string{
		"repo",
		"packages/bar/bar.2.0/opam",
		"packages/foo/foo.1.0/opam",
	} {
		if !names[want] {
			t.Errorf("index is missing %s", want)
		}
	}
	if names[IndexName] {
		t.Error("index contains itself")
	}
}

func tarNames(t *testing.T, gzipped []byte) map[string]bool {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(gzipped))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()

	names := make(map[string]bool)
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names[strings.TrimSuffix(hdr.Name, "/")] = true
	}
	return names
}

func TestArchiveName(t *testing.T) {
	tests := []struct {
		src, want string
	}{
		{"https://example.org/archive/v1.0.tar.gz", "v1.0.tar.gz"},
		{"https://example.org/archive/v1.0.tar.gz?token=x", "v1.0.tar.gz"},
		{"https://example.org/", "pkg.1.0.tar.gz"},
		{"https://example.org/%2e%2e/escape.tar.gz", "escape.tar.gz"}, // decoded, then Base strips the directory part
		{"https://example.org/weird name.tar.gz", "pkg.1.0.tar.gz"},
		{"://nonsense", "pkg.1.0.tar.gz"},
	}
	for _, tt := range tests {
		if got := ArchiveName(tt.src, "pkg", "1.0"); got != tt.want {
			t.Errorf("ArchiveName(%q) = %q, want %q", tt.src, got, tt.want)
		}
	}
}

// ArchiveName output ends up in a URL, in an opam file and in a filesystem
// path, so the one property that must hold for every possible input is that it
// is a single safe path element.
func FuzzArchiveName(f *testing.F) {
	for _, seed := range []string{
		"https://example.org/v1.0.tar.gz",
		"https://example.org/../../etc/passwd",
		"https://example.org/%00.tar.gz",
		"http://[::1]/x",
		"",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, src string) {
		got := ArchiveName(src, "pkg", "1.0")
		if got == "" {
			t.Fatalf("ArchiveName(%q) returned an empty name", src)
		}
		if !safeArchiveName(got) {
			t.Fatalf("ArchiveName(%q) = %q, which is not a safe path element", src, got)
		}
		if filepath.Base(got) != got || strings.Contains(got, "/") {
			t.Fatalf("ArchiveName(%q) = %q, which is not a single element", src, got)
		}
	})
}

func TestMirrorable(t *testing.T) {
	yes := []string{"https://a.example/x.tar.gz", "http://a.example/x.tar.gz"}
	no := []string{"git+https://a.example/x.git", "ftp://a.example/x.tar.gz", "https://a.example", "", "not a url"}
	for _, s := range yes {
		if !Mirrorable(s) {
			t.Errorf("Mirrorable(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if Mirrorable(s) {
			t.Errorf("Mirrorable(%q) = true, want false", s)
		}
	}
}

// TestBuildCorpus generates a mirror from a real opam-repository checkout.
//
// Set OPAMELA_CORPUS to a clone:
//
//	OPAMELA_CORPUS=/path/to/opam-repository go test ./internal/mirror/ -run Corpus -v
func TestBuildCorpus(t *testing.T) {
	root := os.Getenv("OPAMELA_CORPUS")
	if root == "" {
		t.Skip("set OPAMELA_CORPUS to an opam-repository checkout")
	}

	dst := filepath.Join(t.TempDir(), "mirror")
	b := &Builder{BaseURL: "https://opam.internal"}

	start := time.Now()
	stats, err := b.Build(context.Background(), opamrepo.Sources{Base: root}, dst)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Logf("%s in %s", stats, time.Since(start).Round(time.Millisecond))

	if stats.Packages < 1000 {
		t.Fatalf("only %d packages: is %s really an opam-repository?", stats.Packages, root)
	}
	if stats.Rewritten == 0 {
		t.Fatal("nothing was rewritten")
	}

	// Every rewritten package must point at the mirror, and no stray file
	// may have landed one level above a versioned directory.
	packages := filepath.Join(dst, "packages")
	entries, err := os.ReadDir(packages)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if _, err := os.Stat(filepath.Join(packages, e.Name(), "opam")); err == nil {
			t.Fatalf("stray opam file at packages/%s/opam", e.Name())
		}
	}

	if _, err := os.Stat(filepath.Join(dst, IndexName)); err != nil {
		t.Errorf("no index generated: %v", err)
	}

	var checked, pointingAtMirror int
	err = filepath.WalkDir(packages, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "opam" {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		u, err := opamfile.Parse(data)
		if err != nil {
			return nil // sourceless
		}
		checked++
		if strings.HasPrefix(u.Src, "https://opam.internal/download/") {
			pointingAtMirror++
			if strings.ContainsAny(u.Src, "\n\r\t\"") {
				t.Fatalf("%s: rewritten src carries control characters", p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d packages with a source, %d now pointing at the mirror", checked, pointingAtMirror)
	if pointingAtMirror != stats.Rewritten {
		t.Errorf("%d files point at the mirror but Build reported %d rewritten", pointingAtMirror, stats.Rewritten)
	}
}
