package opamfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCorpus runs the parser over a real opam-repository checkout and asserts
// that every file either yields a usable url.src or is legitimately sourceless.
//
// Unit tests prove the parser handles the cases we thought of. This one proves
// it handles the ones we did not. Point it at a clone:
//
//	OPAMELA_CORPUS=/path/to/opam-repository go test ./internal/opamfile/ -run Corpus -v
func TestCorpus(t *testing.T) {
	root := os.Getenv("OPAMELA_CORPUS")
	if root == "" {
		t.Skip("set OPAMELA_CORPUS to an opam-repository checkout")
	}

	var files, withURL, sourceless int
	var failures []string

	packages := filepath.Join(root, "packages")
	err := filepath.WalkDir(packages, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "opam" {
			return err
		}
		// Only versioned package files: packages/<name>/<name>.<version>/opam
		if filepath.Dir(filepath.Dir(path)) == packages {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files++

		u, err := Parse(data)
		switch {
		case errors.Is(err, ErrNoURL):
			sourceless++
			return nil
		case err != nil:
			failures = append(failures, path+": "+err.Error())
			return nil
		}

		withURL++

		// A parsed src must be a plausible, single-line URL.
		if strings.ContainsAny(u.Src, "\n\r\t\"") {
			failures = append(failures, path+": src carries control characters")
		}
		if !strings.Contains(u.Src, "://") {
			failures = append(failures, path+": src is not a URL: "+u.Src)
		}

		// Round-trip: rewriting and re-parsing must give back what we wrote,
		// with the checksums intact.
		const replacement = "https://mirror.invalid/download/x/1.0/a.tar.gz"
		out, err := RewriteSrc(data, replacement)
		if err != nil {
			failures = append(failures, path+": rewrite: "+err.Error())
			return nil
		}
		again, err := Parse(out)
		if err != nil {
			failures = append(failures, path+": re-parse after rewrite: "+err.Error())
			return nil
		}
		if again.Src != replacement {
			failures = append(failures, path+": rewrite produced src "+again.Src)
		}
		if len(again.Checksums) != len(u.Checksums) {
			failures = append(failures, path+": rewrite changed the checksum count")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	t.Logf("%d files: %d with a source, %d sourceless", files, withURL, sourceless)
	if files == 0 {
		t.Fatalf("no opam files under %s", packages)
	}
	for i, f := range failures {
		if i == 20 {
			t.Errorf("... and %d more", len(failures)-20)
			break
		}
		t.Error(f)
	}
}
