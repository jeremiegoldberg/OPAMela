// Package server exposes the mirror over HTTP.
package server

import (
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jeremiegoldberg/opamela/internal/fetch"
	"github.com/jeremiegoldberg/opamela/internal/mirror"
	"github.com/jeremiegoldberg/opamela/internal/opamfile"
	"github.com/jeremiegoldberg/opamela/internal/opamrepo"
)

// State is the pair of trees the server reads, plus the upstream revision they
// were built from.
//
// Two trees, and no database. The generated tree is what opam consumes; the
// pristine checkout still holds every original URL and checksum, so the mapping
// from a mirror URL back to its upstream source stays derivable at any moment.
// Storing that mapping separately would mean keeping two sources of truth in
// step, which is a job nobody does correctly for long.
type State struct {
	MirrorDir string
	Pristine  opamrepo.Sources
	Rev       string
	BuiltAt   time.Time
}

// Server answers opam's requests.
type Server struct {
	cache *fetch.Cache
	log   *slog.Logger
	state atomic.Pointer[State]
}

// New returns a server with no state yet; call SetState once the first build
// has completed.
func New(cache *fetch.Cache, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{cache: cache, log: log}
}

// SetState publishes a freshly built pair of trees.
//
// The swap is a single pointer store, so a rebuild never exposes a half written
// repository: requests in flight finish reading the previous tree, and the next
// ones see the new one.
func (s *Server) SetState(st State) { s.state.Store(&st) }

// Handler returns the routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /download/{package}/{version}/{archive}", s.handleDownload)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /", s.handleRepo)
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	st := s.state.Load()
	if st == nil {
		http.Error(w, "building", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("ok rev=" + st.Rev + " built=" + st.BuiltAt.UTC().Format(time.RFC3339) + "\n"))
}

// handleRepo serves the generated repository: the repo descriptor, the index
// tarball and the rewritten opam files.
func (s *Server) handleRepo(w http.ResponseWriter, r *http.Request) {
	st := s.state.Load()
	if st == nil {
		http.Error(w, "mirror is still building", http.StatusServiceUnavailable)
		return
	}

	clean := cleanRequestPath(r.URL.Path)
	if clean == "" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("opamela: an opam repository mirror\n\n" +
			"  opam repository set-url default " + baseFromRequest(r) + "\n"))
		return
	}

	full := filepath.Join(st.MirrorDir, filepath.FromSlash(clean))
	info, err := os.Stat(full)
	if err != nil || !info.Mode().IsRegular() {
		// Directory listings are of no use to opam and only invite
		// crawlers, so a directory is simply not found.
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, full)
}

// handleDownload resolves a mirror URL back to its upstream source, fetches it
// once, and serves it.
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	st := s.state.Load()
	if st == nil {
		http.Error(w, "mirror is still building", http.StatusServiceUnavailable)
		return
	}

	name := r.PathValue("package")
	version := r.PathValue("version")
	archive := r.PathValue("archive")

	pkg, err := opamrepo.Find(st.Pristine.Roots(), name, version)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	data, err := os.ReadFile(pkg.OpamPath())
	if err != nil {
		s.log.Error("reading opam file", "path", pkg.OpamPath(), "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	u, err := opamfile.Parse(data)
	if err != nil || !mirror.Mirrorable(u.Src) {
		http.NotFound(w, r)
		return
	}

	// The archive name is derived, not trusted: accepting an arbitrary name
	// here would let one upstream source be cached under many keys.
	if want := mirror.ArchiveName(u.Src, pkg.Name, pkg.Version); archive != want {
		http.NotFound(w, r)
		return
	}

	key := pkg.Name + "/" + pkg.Version + "/" + archive
	local, err := s.cache.Get(r.Context(), key, u.Src, u.Checksums)
	switch {
	case errors.Is(err, fetch.ErrChecksumMismatch):
		s.log.Error("refusing to serve archive", "key", key, "err", err)
		http.Error(w, "upstream archive failed checksum verification", http.StatusBadGateway)
		return
	case errors.Is(err, fs.ErrNotExist):
		http.NotFound(w, r)
		return
	case err != nil:
		s.log.Error("fetching archive", "key", key, "err", err)
		http.Error(w, "upstream fetch failed", http.StatusBadGateway)
		return
	}

	f, err := os.Open(local)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, archive, info.ModTime(), f)
}

// cleanRequestPath reduces a request path to a slash separated relative path with no
// escaping element, or "" for the root.
func cleanRequestPath(p string) string {
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return ""
	}
	parts := strings.Split(p, "/")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		switch part {
		case "", ".":
			continue
		case "..":
			return "" // refuse rather than resolve
		default:
			out = append(out, part)
		}
	}
	return strings.Join(out, "/")
}

func baseFromRequest(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}
