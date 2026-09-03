// Package mirror turns an opam-repository checkout into a repository whose
// packages point at this server.
//
// The whole idea of opamela lives in this package. An opam repository is not a
// package store: it is a directory of URLs pointing at several thousand
// unrelated servers. That is why putting an HTTP cache in front of one caches
// nothing worth caching, and why the only way to become the single path a build
// takes is to rewrite the directory itself.
package mirror

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/jeremiegoldberg/opamela/internal/opamfile"
	"github.com/jeremiegoldberg/opamela/internal/opamrepo"
)

// IndexName is the file opam fetches when a repository is served over HTTP.
// Serving the tree alone is not enough, which is a detail worth knowing before
// wondering why a perfectly good directory of opam files does nothing.
const IndexName = "index.tar.gz"

// Stats reports what a build did.
type Stats struct {
	Packages    int // versioned packages seen
	Rewritten   int // now pointing at this server
	Sourceless  int // no url.src: conf-* and virtual packages
	Passthrough int // a source we cannot serve as a file, left untouched
}

func (s Stats) String() string {
	return fmt.Sprintf("%d packages: %d rewritten, %d sourceless, %d passed through",
		s.Packages, s.Rewritten, s.Sourceless, s.Passthrough)
}

// Builder generates a rewritten repository.
type Builder struct {
	// BaseURL is the public root of this server, without a trailing slash,
	// for example "https://opam.internal".
	BaseURL string

	// Workers bounds concurrency. Zero means one per CPU.
	Workers int
}

// Build writes a rewritten copy of the source repositories into dstRoot,
// replacing whatever was there, and generates the index tarball.
//
// Packages are independent, so they are built in parallel. Note what is not
// here: no shared database, no shared handle, nothing to serialise. Each worker
// reads one file and writes another. Concurrency is safe by construction rather
// than by locking, which is the cheapest kind of safe.
func (b *Builder) Build(ctx context.Context, src opamrepo.Sources, dstRoot string) (Stats, error) {
	base := strings.TrimSuffix(b.BaseURL, "/")
	if base == "" {
		return Stats{}, errors.New("mirror: empty BaseURL")
	}

	roots := src.Roots()
	if len(roots) == 0 {
		return Stats{}, errors.New("mirror: no source repository")
	}

	packages, err := opamrepo.List(roots)
	if err != nil {
		return Stats{}, err
	}

	if err := os.RemoveAll(dstRoot); err != nil {
		return Stats{}, err
	}
	if err := os.MkdirAll(dstRoot, 0o755); err != nil {
		return Stats{}, err
	}
	if err := copyRepoFile(src.Base, dstRoot); err != nil {
		return Stats{}, err
	}

	workers := b.Workers
	if workers <= 0 {
		workers = runtime.NumCPU()
	}

	var (
		rewritten, sourceless, passthrough atomic.Int64
		firstErr                           error
		errOnce                            sync.Once
	)

	jobs := make(chan opamrepo.Package)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for pkg := range jobs {
				kind, err := buildPackage(pkg, dstRoot, base)
				if err != nil {
					errOnce.Do(func() { firstErr = err })
					return
				}
				switch kind {
				case rewrittenPackage:
					rewritten.Add(1)
				case sourcelessPackage:
					sourceless.Add(1)
				case passthroughPackage:
					passthrough.Add(1)
				}
			}
		}()
	}

feed:
	for _, pkg := range packages {
		select {
		case jobs <- pkg:
		case <-ctx.Done():
			firstErr = ctx.Err()
			break feed
		}
	}
	close(jobs)
	wg.Wait()

	if firstErr != nil {
		return Stats{}, firstErr
	}

	if err := writeIndex(dstRoot); err != nil {
		return Stats{}, fmt.Errorf("writing %s: %w", IndexName, err)
	}

	return Stats{
		Packages:    len(packages),
		Rewritten:   int(rewritten.Load()),
		Sourceless:  int(sourceless.Load()),
		Passthrough: int(passthrough.Load()),
	}, nil
}

type packageKind int

const (
	rewrittenPackage packageKind = iota
	sourcelessPackage
	passthroughPackage
)

func buildPackage(pkg opamrepo.Package, dstRoot, base string) (packageKind, error) {
	dstDir := filepath.Join(dstRoot, pkg.RelDir())
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return 0, err
	}

	// Patches and other side files travel with the package untouched.
	if err := copyExtras(pkg.Dir, dstDir); err != nil {
		return 0, err
	}

	data, err := os.ReadFile(pkg.OpamPath())
	if err != nil {
		return 0, err
	}

	f, _ := opamfile.Parse(data) // Parse never returns an error

	// Rewrite the url source and every mirrorable extra-source in one pass, so
	// that the patches opam fetches at build time also come through the mirror
	// instead of straight from the public internet.
	out, err := opamfile.RewriteSources(data, f, func(s opamfile.Source) string {
		if !Mirrorable(s.Src) {
			return ""
		}
		archive := ArchiveNameFor(s, pkg.Name, pkg.Version)
		return base + DownloadPath(pkg.Name, pkg.Version, archive)
	})
	if err != nil {
		return 0, fmt.Errorf("%s: %w", pkg.OpamPath(), err)
	}

	// Classify the package by its own source, for the build summary. Its
	// extra-sources were rewritten above regardless of this.
	var kind packageKind
	switch {
	case f.URL == nil:
		// conf-* and virtual packages have no source archive.
		kind = sourcelessPackage
	case Mirrorable(f.URL.Src):
		kind = rewrittenPackage
	default:
		// git+https and the like cannot be served as an archive. Leaving the
		// url alone is the honest outcome, and a mirror that pretended
		// otherwise would hand opam a file where it expects a clone.
		kind = passthroughPackage
	}

	// The destination is the versioned package directory. Writing one level
	// up produces packages/<name>/opam, a file opam never reads, and a
	// mirror that silently redirects nothing at all.
	return kind, os.WriteFile(filepath.Join(dstDir, "opam"), out, 0o644)
}

// DownloadPath is the server path a rewritten package points at.
func DownloadPath(pkg, version, archive string) string {
	return "/download/" + pkg + "/" + version + "/" + archive
}

// Mirrorable reports whether a source URL can be served as a plain file.
func Mirrorable(src string) bool {
	u, err := url.Parse(src)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.Path != ""
}

// ArchiveName is the file name a rewritten package advertises.
//
// It is derived from the upstream URL so that opam's own archive cache keeps
// recognisable names, and it is always a single safe path element: this string
// ends up in a URL, in an opam file and in a filesystem path.
func ArchiveName(src, pkg, version string) string {
	fallback := pkg + "." + version + ".tar.gz"

	u, err := url.Parse(src)
	if err != nil {
		return fallback
	}
	name := path.Base(u.Path)
	if name == "" || name == "." || name == "/" || name == ".." {
		return fallback
	}
	if !safeArchiveName(name) {
		return fallback
	}
	return name
}

// ArchiveNameFor is the file name a rewritten source advertises. For the url
// section it comes from the upstream URL; for an extra-source it is the
// section's label, which opam guarantees to be a filename unique within the
// package. The rare case where an extra-source label collides with the url
// archive name resolves in favour of the url, since the server tries the url
// first; such a collision has not been observed in opam-repository.
func ArchiveNameFor(s opamfile.Source, pkg, version string) string {
	if s.IsExtra() && safeArchiveName(s.Name) {
		return s.Name
	}
	return ArchiveName(s.Src, pkg, version)
}

func safeArchiveName(s string) bool {
	if len(s) > 200 || strings.Contains(s, "..") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '-', r == '_', r == '+', r == '~':
		default:
			return false
		}
	}
	return true
}

// copyRepoFile carries the repository's own descriptor across, synthesising a
// minimal one if the upstream checkout has none.
func copyRepoFile(srcRoot, dstRoot string) error {
	data, err := os.ReadFile(filepath.Join(srcRoot, "repo"))
	if errors.Is(err, fs.ErrNotExist) {
		data = []byte("opam-version: \"2.0\"\n")
	} else if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dstRoot, "repo"), data, 0o644)
}

// copyExtras copies everything in a package directory except its opam file.
func copyExtras(srcDir, dstDir string) error {
	return filepath.WalkDir(srcDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, p)
		if err != nil {
			return err
		}
		if rel == "." || rel == "opam" {
			return nil
		}
		dst := filepath.Join(dstDir, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return copyFile(p, dst)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// writeIndex packs the generated tree into index.tar.gz.
//
// The output is deterministic: paths are sorted and timestamps and ownership
// are zeroed. Two builds of the same upstream revision therefore produce byte
// identical tarballs, which makes "did the mirror actually change?" a question
// with an answer.
func writeIndex(root string) error {
	var paths []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == "." || rel == IndexName {
			return nil
		}
		if d.IsDir() || d.Type().IsRegular() {
			paths = append(paths, rel)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(paths)

	tmp := filepath.Join(root, IndexName+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}

	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	for _, rel := range paths {
		full := filepath.Join(root, rel)
		info, err := os.Lstat(full)
		if err != nil {
			f.Close()
			return err
		}

		hdr := &tar.Header{
			Name:   filepath.ToSlash(rel),
			Mode:   0o644,
			Format: tar.FormatPAX,
		}
		if info.IsDir() {
			hdr.Typeflag, hdr.Mode, hdr.Name = tar.TypeDir, 0o755, hdr.Name+"/"
		} else {
			hdr.Typeflag, hdr.Size = tar.TypeReg, info.Size()
		}
		if err := tw.WriteHeader(hdr); err != nil {
			f.Close()
			return err
		}
		if info.IsDir() {
			continue
		}

		in, err := os.Open(full)
		if err != nil {
			f.Close()
			return err
		}
		if _, err := io.Copy(tw, in); err != nil {
			in.Close()
			f.Close()
			return err
		}
		in.Close()
	}

	if err := tw.Close(); err != nil {
		f.Close()
		return err
	}
	if err := gz.Close(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(root, IndexName))
}
