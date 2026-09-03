package opamfile

import (
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

		u, _ := Parse(data)
		if u.URL == nil {
			sourceless++
			return nil
		}

		withURL++

		// A parsed src must be a plausible, single-line URL.
		if strings.ContainsAny(u.URL.Src, "\n\r\t\"") {
			failures = append(failures, path+": src carries control characters")
		}
		if !strings.Contains(u.URL.Src, "://") {
			failures = append(failures, path+": src is not a URL: "+u.URL.Src)
		}

		// Round-trip: rewriting and re-parsing must give back what we wrote,
		// with the checksums intact.
		const replacement = "https://mirror.invalid/download/x/1.0/a.tar.gz"
		out, err := RewriteSrc(data, replacement)
		if err != nil {
			failures = append(failures, path+": rewrite: "+err.Error())
			return nil
		}
		again, _ := Parse(out)
		if again.URL == nil {
			failures = append(failures, path+": rewrite dropped the url section")
			return nil
		}
		if again.URL.Src != replacement {
			failures = append(failures, path+": rewrite produced src "+again.URL.Src)
		}
		if len(again.URL.Checksums) != len(u.URL.Checksums) {
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
