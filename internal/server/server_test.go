package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jeremiegoldberg/opamela/internal/fetch"
	"github.com/jeremiegoldberg/opamela/internal/mirror"
	"github.com/jeremiegoldberg/opamela/internal/opamfile"
	"github.com/jeremiegoldberg/opamela/internal/opamrepo"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// harness is a whole mirror: a fake upstream serving archives, a fake
// opam-repository pointing at it, a generated mirror, and this server.
type harness struct {
	upstreamHits map[string]int
	archives     map[string][]byte
	mirrorURL    string
	client       *http.Client
}

func newHarness(t *testing.T, archives map[string][]byte, tamper bool) *harness {
	t.Helper()

	h := &harness{archives: archives, upstreamHits: map[string]int{}, client: &http.Client{Timeout: 10 * time.Second}}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := archives[filepath.Base(r.URL.Path)]
		if !ok {
			http.NotFound(w, r)
			return
		}
		h.upstreamHits[filepath.Base(r.URL.Path)]++
		if tamper {
			body = append([]byte("tampered "), body...)
		}
		w.Write(body)
	}))
	t.Cleanup(upstream.Close)

	// A fake opam-repository whose packages point at that upstream.
	pristine := t.TempDir()
	for name, body := range archives {
		sum := sha256.Sum256(body)
		content := `opam-version: "2.0"
synopsis: "test package"
url {
  src: "` + upstream.URL + `/dist/` + name + `"
  checksum: "sha256=` + hex.EncodeToString(sum[:]) + `"
}
`
		dir := filepath.Join(pristine, "packages", "pkg-"+name, "pkg-"+name+".1.0")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "opam"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// The mirror has to be generated with the URL it will actually answer on,
	// so the listener comes up before the build.
	ts := httptest.NewUnstartedServer(nil)
	baseURL := "http://" + ts.Listener.Addr().String()

	sources := opamrepo.Sources{Base: pristine}
	mirrorDir := filepath.Join(t.TempDir(), "mirror")
	b := &mirror.Builder{BaseURL: baseURL, Workers: 2}
	if _, err := b.Build(context.Background(), sources, mirrorDir); err != nil {
		t.Fatalf("Build: %v", err)
	}

	cache := fetch.New(context.Background(), t.TempDir(), nil, 10*time.Second, quietLog())
	srv := New(cache, quietLog())
	srv.SetState(State{
		MirrorDir: mirrorDir,
		Pristine:  sources,
		Rev:       "test",
		BuiltAt:   time.Now(),
	})

	ts.Config.Handler = srv.Handler()
	ts.Start()
	t.Cleanup(ts.Close)

	h.mirrorURL = baseURL
	return h
}

func (h *harness) get(t *testing.T, url string) (*http.Response, []byte) {
	t.Helper()
	resp, err := h.client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

// TestEndToEnd walks the path opam walks: read the repository, read a package
// file, follow the URL it advertises, get the archive.
//
// This is the test the original prototype would have failed. It served a
// repository whose rewritten files were written to a path opam never reads, so
// every build silently went back to fetching from GitHub. Nothing short of
// following the advertised URL catches that.
func TestEndToEnd(t *testing.T) {
	body := []byte("this is the archive content")
	h := newHarness(t, map[string][]byte{"thing-1.0.tar.gz": body}, false)

	// The repository descriptor and the index opam actually fetches.
	if resp, _ := h.get(t, h.mirrorURL+"/repo"); resp.StatusCode != http.StatusOK {
		t.Errorf("GET /repo = %s", resp.Status)
	}
	resp, index := h.get(t, h.mirrorURL+"/"+mirror.IndexName)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /%s = %s", mirror.IndexName, resp.Status)
	}
	if len(index) == 0 {
		t.Fatal("empty index tarball")
	}

	// The package file, as opam reads it.
	resp, opamFile := h.get(t, h.mirrorURL+"/packages/pkg-thing-1.0.tar.gz/pkg-thing-1.0.tar.gz.1.0/opam")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET package file = %s", resp.Status)
	}
	u, _ := opamfile.Parse(opamFile)
	if u.URL == nil {
		t.Fatal("served opam file has no url section")
	}

	// It must point at us, not upstream.
	if got := u.URL.Src; got[:len(h.mirrorURL)] != h.mirrorURL {
		t.Fatalf("served package points at %q, not at the mirror", got)
	}

	// Follow it, exactly as opam would.
	resp, archive := h.get(t, u.URL.Src)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %s", u.URL.Src, resp.Status)
	}
	if string(archive) != string(body) {
		t.Errorf("archive content = %q, want %q", archive, body)
	}

	// And a second build gets it from disk.
	h.get(t, u.URL.Src)
	if n := h.upstreamHits["thing-1.0.tar.gz"]; n != 1 {
		t.Errorf("upstream was hit %d times, want 1", n)
	}
}

func TestDownloadRefusesTamperedUpstream(t *testing.T) {
	h := newHarness(t, map[string][]byte{"thing-1.0.tar.gz": []byte("the real content")}, true)

	url := h.mirrorURL + mirror.DownloadPath("pkg-thing-1.0.tar.gz", "1.0", "thing-1.0.tar.gz")
	resp, _ := h.get(t, url)
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %s, want 502 for content that fails its own checksum", resp.Status)
	}
}

func TestDownloadRejectsUnexpectedNames(t *testing.T) {
	h := newHarness(t, map[string][]byte{"thing-1.0.tar.gz": []byte("content")}, false)

	for _, path := range []string{
		mirror.DownloadPath("pkg-thing-1.0.tar.gz", "1.0", "something-else.tar.gz"),
		mirror.DownloadPath("pkg-thing-1.0.tar.gz", "9.9", "thing-1.0.tar.gz"),
		mirror.DownloadPath("no-such-package", "1.0", "thing-1.0.tar.gz"),
		"/download/pkg-thing-1.0.tar.gz/1.0/..%2f..%2f..%2fetc%2fpasswd",
		"/download/..%2f..%2fetc/1.0/passwd",
	} {
		resp, _ := h.get(t, h.mirrorURL+path)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %s, want 404", path, resp.Status)
		}
	}
}

func TestRepoServingRejectsTraversalAndListings(t *testing.T) {
	h := newHarness(t, map[string][]byte{"thing-1.0.tar.gz": []byte("content")}, false)

	for _, path := range []string{
		"/../../etc/passwd",
		"/packages/../../../etc/passwd",
		"/packages", // a directory: no listings
		"/packages/pkg-thing-1.0.tar.gz",
	} {
		resp, _ := h.get(t, h.mirrorURL+path)
		if resp.StatusCode == http.StatusOK {
			t.Errorf("GET %s = 200, want a refusal", path)
		}
	}
}

func TestUnavailableBeforeFirstBuild(t *testing.T) {
	srv := New(fetch.New(context.Background(), t.TempDir(), nil, time.Second, quietLog()), quietLog())
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, path := range []string{"/healthz", "/repo", "/download/foo/1.0/a.tar.gz"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("GET %s = %s, want 503 while building", path, resp.Status)
		}
	}
}

func TestCleanRequestPath(t *testing.T) {
	tests := map[string]string{
		"/":                       "",
		"":                        "",
		"/repo":                   "repo",
		"/packages/a/a.1/opam":    "packages/a/a.1/opam",
		"//packages//a//":         "packages/a",
		"/./repo":                 "repo",
		"/../etc/passwd":          "",
		"/packages/../../etc/pwd": "",
	}
	for in, want := range tests {
		if got := cleanRequestPath(in); got != want {
			t.Errorf("cleanRequestPath(%q) = %q, want %q", in, got, want)
		}
	}
}
