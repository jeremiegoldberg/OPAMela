// Package fetch downloads package archives once and keeps them on disk.
package fetch

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jeremiegoldberg/opamela/internal/opamfile"
)

// ErrChecksumMismatch reports that upstream served content the package does not
// vouch for.
var ErrChecksumMismatch = errors.New("fetch: checksum mismatch")

// Cache is a content store for package archives.
//
// It guarantees three things that are easy to get wrong and expensive to debug:
// a file that appears in the cache is complete, a file that appears in the
// cache matched its declared checksum, and a hundred simultaneous requests for
// a cold archive cause one download.
type Cache struct {
	dir     string
	client  *http.Client
	log     *slog.Logger
	base    context.Context
	timeout time.Duration

	mu       sync.Mutex
	inflight map[string]*call
}

type call struct {
	done chan struct{}
	err  error
}

// New returns a cache storing archives under dir.
//
// base bounds the lifetime of downloads in flight and timeout bounds each one.
// Neither is taken from the requesting client, for the reason explained on Get.
func New(base context.Context, dir string, client *http.Client, timeout time.Duration, log *slog.Logger) *Cache {
	if client == nil {
		client = http.DefaultClient
	}
	if log == nil {
		log = slog.Default()
	}
	if base == nil {
		base = context.Background()
	}
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	return &Cache{
		dir:      dir,
		client:   client,
		log:      log,
		base:     base,
		timeout:  timeout,
		inflight: make(map[string]*call),
	}
}

// Path is where key is or would be stored. Callers must have validated key.
func (c *Cache) Path(key string) string {
	return filepath.Join(c.dir, filepath.FromSlash(key))
}

// Get returns the local path of the archive for key, downloading it from src on
// first use. ctx bounds how long this caller is prepared to wait, and nothing
// more.
//
// A download runs under the cache's own context, not the caller's. A build that
// times out and disconnects while a 200 MB archive is halfway down should not
// take the download with it: nine other builds are queued behind it, and the
// next one would start from zero. So the caller can walk away; the transfer
// finishes.
//
// Checksums come from the package's own opam file, so verification here is a
// check against upstream, not against the client. A package that declares no
// checksum is fetched and logged: a mirror cannot be more trustworthy than the
// repository it mirrors, and pretending otherwise would only hide the fact.
func (c *Cache) Get(ctx context.Context, key, src string, sums []opamfile.Checksum) (string, error) {
	dst := c.Path(key)
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}

	c.mu.Lock()
	cl, running := c.inflight[key]
	if !running {
		cl = &call{done: make(chan struct{})}
		c.inflight[key] = cl
		go func() {
			ctx, cancel := context.WithTimeout(c.base, c.timeout)
			defer cancel()

			cl.err = c.download(ctx, dst, src, sums)

			c.mu.Lock()
			delete(c.inflight, key)
			c.mu.Unlock()
			close(cl.done)
		}()
	}
	c.mu.Unlock()

	select {
	case <-cl.done:
		if cl.err != nil {
			return "", cl.err
		}
		return dst, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// download writes src to dst, verifying it before it becomes visible.
//
// The archive is streamed into a temporary file in the destination directory
// and hashed on the way through, then renamed into place. Nothing partial, and
// nothing unverified, ever carries the name of a real archive: a truncated
// download that keeps the final name is a build failure that survives every
// retry until somebody thinks to delete a file by hand.
func (c *Cache) download(ctx context.Context, dst, src string, sums []opamfile.Checksum) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "opamela")

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetching %s: %w", src, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetching %s: upstream returned %s", src, resp.Status)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".partial-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName) // no-op once renamed
	}()

	hashers := make(map[string]hash.Hash, len(sums))
	writers := []io.Writer{tmp}
	for _, s := range sums {
		if _, seen := hashers[s.Kind]; seen {
			continue
		}
		h := newHash(s.Kind)
		if h == nil {
			continue
		}
		hashers[s.Kind] = h
		writers = append(writers, h)
	}

	n, err := io.Copy(io.MultiWriter(writers...), resp.Body)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", src, err)
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if len(hashers) == 0 {
		c.log.Warn("archive has no usable checksum, serving unverified",
			"src", src, "bytes", n)
	}
	for _, s := range sums {
		h, ok := hashers[s.Kind]
		if !ok {
			continue
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != s.Hex {
			return fmt.Errorf("%w for %s: %s upstream=%s expected=%s",
				ErrChecksumMismatch, src, s.Kind, got, s.Hex)
		}
	}

	if err := os.Rename(tmpName, dst); err != nil {
		return err
	}
	c.log.Info("cached archive", "key", filepath.Base(dst), "bytes", n, "src", src)
	return nil
}

func newHash(kind string) hash.Hash {
	switch kind {
	case "md5":
		return md5.New()
	case "sha256":
		return sha256.New()
	case "sha512":
		return sha512.New()
	default:
		return nil
	}
}
