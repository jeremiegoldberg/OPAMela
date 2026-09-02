package fetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jeremiegoldberg/opamela/internal/opamfile"
)

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func sha256Of(b []byte) opamfile.Checksum {
	sum := sha256.Sum256(b)
	return opamfile.Checksum{Kind: "sha256", Hex: hex.EncodeToString(sum[:])}
}

func newCache(t *testing.T) *Cache {
	t.Helper()
	return New(context.Background(), t.TempDir(), nil, 30*time.Second, quietLog())
}

func TestGetDownloadsAndCaches(t *testing.T) {
	body := []byte("the archive bytes")
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write(body)
	}))
	defer up.Close()

	c := newCache(t)
	sums := []opamfile.Checksum{sha256Of(body)}

	for i := 0; i < 3; i++ {
		path, err := c.Get(context.Background(), "foo/1.0/a.tar.gz", up.URL+"/a.tar.gz", sums)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(body) {
			t.Errorf("call %d: content = %q", i, got)
		}
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("upstream was hit %d times, want 1", n)
	}
}

// A wrong archive must not merely fail: it must leave nothing behind. The
// prototype this replaces created the destination file before downloading, so
// a failed fetch cached an empty file permanently, and every retry served it.
func TestChecksumMismatchLeavesNothingCached(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("tampered content"))
	}))
	defer up.Close()

	c := newCache(t)
	sums := []opamfile.Checksum{sha256Of([]byte("the content the package vouches for"))}

	_, err := c.Get(context.Background(), "foo/1.0/a.tar.gz", up.URL+"/a.tar.gz", sums)
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("err = %v, want ErrChecksumMismatch", err)
	}
	assertNothingCached(t, c, "foo/1.0/a.tar.gz")
}

func TestUpstreamErrorLeavesNothingCached(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer up.Close()

	c := newCache(t)
	if _, err := c.Get(context.Background(), "foo/1.0/a.tar.gz", up.URL+"/a.tar.gz", nil); err == nil {
		t.Fatal("err = nil, want a failure")
	}
	assertNothingCached(t, c, "foo/1.0/a.tar.gz")
}

// A connection dropped mid-transfer must not leave a truncated archive under
// the real name, which would be a build failure that survives every retry.
func TestTruncatedTransferLeavesNothingCached(t *testing.T) {
	body := make([]byte, 1<<16)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Write(body[:1024])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler) // drop the connection
	}))
	defer up.Close()

	c := newCache(t)
	if _, err := c.Get(context.Background(), "foo/1.0/a.tar.gz", up.URL+"/a.tar.gz", []opamfile.Checksum{sha256Of(body)}); err == nil {
		t.Fatal("err = nil, want a failure")
	}
	assertNothingCached(t, c, "foo/1.0/a.tar.gz")
}

func assertNothingCached(t *testing.T, c *Cache, key string) {
	t.Helper()
	if _, err := os.Stat(c.Path(key)); err == nil {
		t.Fatalf("%s was cached despite the failure", key)
	}
	// Nor should a partial file be left lying around.
	entries, err := os.ReadDir(filepath.Dir(c.Path(key)))
	if err != nil {
		return // the directory may legitimately not exist
	}
	for _, e := range entries {
		t.Errorf("leftover file in cache directory: %s", e.Name())
	}
}

// Many builds start at once and ask for the same cold archive. One download.
func TestConcurrentRequestsCauseOneDownload(t *testing.T) {
	body := []byte("shared archive")
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(80 * time.Millisecond) // long enough for everyone to pile up
		w.Write(body)
	}))
	defer up.Close()

	c := newCache(t)
	sums := []opamfile.Checksum{sha256Of(body)}

	const callers = 25
	var wg sync.WaitGroup
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.Get(context.Background(), "foo/1.0/a.tar.gz", up.URL+"/a.tar.gz", sums)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: %v", i, err)
		}
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("upstream was hit %d times, want 1", n)
	}
}

// A caller that gives up must not take the download with it: the next caller
// would otherwise start again from zero.
func TestAbandonedCallerDoesNotCancelTheDownload(t *testing.T) {
	body := []byte("slow archive")
	var hits atomic.Int64
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-release
		w.Write(body)
	}))
	defer up.Close()

	c := newCache(t)
	sums := []opamfile.Checksum{sha256Of(body)}

	giveUp, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Get(giveUp, "foo/1.0/a.tar.gz", up.URL+"/a.tar.gz", sums); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}

	close(release)

	// The transfer was still running, so a patient caller gets the file
	// without a second request upstream.
	path, err := c.Get(context.Background(), "foo/1.0/a.tar.gz", up.URL+"/a.tar.gz", sums)
	if err != nil {
		t.Fatalf("second caller: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != string(body) {
		t.Errorf("content = %q", got)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("upstream was hit %d times, want 1", n)
	}
}

func TestUnverifiableArchiveIsStillServed(t *testing.T) {
	body := []byte("no checksum declared upstream")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer up.Close()

	c := newCache(t)
	path, err := c.Get(context.Background(), "foo/1.0/a.tar.gz", up.URL+"/a.tar.gz", nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != string(body) {
		t.Errorf("content = %q", got)
	}
}

func TestAllDeclaredChecksumsAreVerified(t *testing.T) {
	body := []byte("content")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer up.Close()

	c := newCache(t)
	// A correct sha256 alongside a wrong md5: opam declares both, so both
	// have to hold.
	sums := []opamfile.Checksum{
		sha256Of(body),
		{Kind: "md5", Hex: "00000000000000000000000000000000"},
	}
	if _, err := c.Get(context.Background(), "foo/1.0/a.tar.gz", up.URL+"/a.tar.gz", sums); !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("err = %v, want ErrChecksumMismatch", err)
	}
}
