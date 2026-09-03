package opamfile

import (
	"strings"
	"testing"
)

// Taken from opam-repository: a single md5, and a src value wrapped onto the
// following line.
const wrappedSrc = `opam-version: "2.0"
maintainer: "Danny Willems <contact@danny-willems.be>"
homepage: "https://github.com/dannywillems/ocaml-cordova-plugin-fcm"
bug-reports:
  "https://github.com/dannywillems/ocaml-cordova-plugin-fcm/issues"
license: "LGPL-3.0-only WITH OCaml-LGPL-linking-exception"
build: [make "build"]
depends: [
  "ocaml" {>= "4.03.0"}
  "gen_js_api"
]
synopsis: "Binding OCaml to cordova-plugin-fcm using gen_js_api."
url {
  src:
    "https://github.com/dannywillems/ocaml-cordova-plugin-fcm/archive/v1.0.zip"
  checksum: "md5=f43612d7e05496ff5863c40b5e8638df"
}
`

// Taken from opam-repository: a checksum list.
const checksumList = `opam-version: "2.0"
synopsis: "Unix.LargeFile bindings"
url {
  src: "https://github.com/savonet/ocaml-posix/archive/v2.0.0.tar.gz"
  checksum: [
    "md5=2c186aa5161b72208a870d5710fb6208"
    "sha512=d583c3d386865eab7575fc4f1976c17294bad2ee5037327cb5c3075965788170e652b7b9b9f660ef25f71558553fbcc47734b971e3c9f41627cc573d75d2fb54"
  ]
}
`

func TestParseWrappedSrc(t *testing.T) {
	u, err := Parse([]byte(wrappedSrc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := "https://github.com/dannywillems/ocaml-cordova-plugin-fcm/archive/v1.0.zip"
	if u.URL.Src != want {
		t.Errorf("Src = %q, want %q", u.URL.Src, want)
	}
	if len(u.URL.Checksums) != 1 || u.URL.Checksums[0].Kind != "md5" {
		t.Fatalf("Checksums = %v, want one md5", u.URL.Checksums)
	}
	if got := u.URL.Checksums[0].Hex; got != "f43612d7e05496ff5863c40b5e8638df" {
		t.Errorf("md5 = %q", got)
	}
}

func TestParseChecksumList(t *testing.T) {
	u, err := Parse([]byte(checksumList))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(u.URL.Checksums) != 2 {
		t.Fatalf("got %d checksums, want 2: %v", len(u.URL.Checksums), u.URL.Checksums)
	}
	if u.URL.Checksums[0].Kind != "md5" || u.URL.Checksums[1].Kind != "sha512" {
		t.Errorf("kinds = %q, %q", u.URL.Checksums[0].Kind, u.URL.Checksums[1].Kind)
	}
}

// A description may contain braces and quotes. A brace counter that does not
// understand triple-quoted strings loses the url section entirely.
func TestParseDescriptionWithBraces(t *testing.T) {
	const in = `opam-version: "2.0"
description: """
Handles records like { a = 1; b = "two" } and unbalanced braces: } } {
"""
url {
  src: "https://example.org/pkg-1.0.tar.gz"
  checksum: "sha256=abc"
}
`
	u, err := Parse([]byte(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if u.URL.Src != "https://example.org/pkg-1.0.tar.gz" {
		t.Errorf("Src = %q", u.URL.Src)
	}
}

// extra-source sections carry src and checksum too, and are not the package
// source. Searching for the first `src:` in the file picks the wrong one.
func TestParseIgnoresExtraSource(t *testing.T) {
	const in = `opam-version: "2.0"
extra-source "fix.patch" {
  src: "https://example.org/patches/fix.patch"
  checksum: "sha256=deadbeef"
}
url {
  src: "https://example.org/real-1.0.tar.gz"
  checksum: "sha256=cafe"
}
`
	u, err := Parse([]byte(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if u.URL.Src != "https://example.org/real-1.0.tar.gz" {
		t.Errorf("Src = %q, want the url section, not extra-source", u.URL.Src)
	}
}

func TestParseNoURL(t *testing.T) {
	const in = `opam-version: "2.0"
synopsis: "Virtual package relying on a system installation"
depends: ["conf-pkg-config" {build}]
`
	f, _ := Parse([]byte(in))
	if f.URL != nil {
		t.Errorf("URL = %+v, want nil for a sourceless package", f.URL)
	}
}

func TestRewriteSrcPreservesEverythingElse(t *testing.T) {
	out, err := RewriteSrc([]byte(checksumList), "https://mirror.internal/download/posix-base/2.0.0/v2.0.0.tar.gz")
	if err != nil {
		t.Fatalf("RewriteSrc: %v", err)
	}

	u, err := Parse(out)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if u.URL.Src != "https://mirror.internal/download/posix-base/2.0.0/v2.0.0.tar.gz" {
		t.Errorf("Src = %q", u.URL.Src)
	}

	// The checksums are the reason a rewritten repository is still safe to
	// build from. They must survive the rewrite byte for byte.
	if len(u.URL.Checksums) != 2 {
		t.Fatalf("checksums lost: %v", u.URL.Checksums)
	}
	for _, keep := range []string{
		`synopsis: "Unix.LargeFile bindings"`,
		"md5=2c186aa5161b72208a870d5710fb6208",
		"sha512=d583c3d386865eab7575fc4f1976c17294bad2ee5037327cb5c3075965788170e652b7b9b9f660ef25f71558553fbcc47734b971e3c9f41627cc573d75d2fb54",
	} {
		if !strings.Contains(string(out), keep) {
			t.Errorf("rewrite dropped %q", keep)
		}
	}
	if strings.Contains(string(out), "github.com/savonet") {
		t.Error("rewrite left the upstream URL behind")
	}
}

func TestRewriteSrcOnWrappedValue(t *testing.T) {
	out, err := RewriteSrc([]byte(wrappedSrc), "https://mirror.internal/download/cordova-plugin-fcm/1.0/v1.0.zip")
	if err != nil {
		t.Fatalf("RewriteSrc: %v", err)
	}
	u, err := Parse(out)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if u.URL.Src != "https://mirror.internal/download/cordova-plugin-fcm/1.0/v1.0.zip" {
		t.Errorf("Src = %q", u.URL.Src)
	}
}

// Regression test. The prototype this replaces derived the archive name from
// an untrimmed subprocess output, so it wrote
//
//	src: "http://host/download/pkg/v1.0.zip
//	"
//
// a URL with a newline inside the quotes. It parsed, and resolved to nothing.
func TestRewriteSrcRejectsControlCharacters(t *testing.T) {
	for _, bad := range []string{
		"https://mirror.internal/download/pkg/1.0/v1.0.zip\n",
		"https://mirror.internal/download/pkg/1.0/v1.0.zip\r\n",
		"https://mirror.internal/download/pkg/1.0/\tv1.0.zip",
		`https://mirror.internal/download/pkg/1.0/v1.0.zip" evil: "yes`,
		`https://mirror.internal/download/pkg\1.0/v1.0.zip`,
	} {
		if _, err := RewriteSrc([]byte(checksumList), bad); err == nil {
			t.Errorf("RewriteSrc(%q) = nil error, want rejection", bad)
		}
	}
	if _, err := RewriteSrc([]byte(checksumList), ""); err == nil {
		t.Error("RewriteSrc(\"\") = nil error, want rejection")
	}
}

func TestParseIsIdempotentUnderRewrite(t *testing.T) {
	data := []byte(checksumList)
	for i := 0; i < 3; i++ {
		out, err := RewriteSrc(data, "https://mirror.internal/download/p/1.0/a.tar.gz")
		if err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		if i > 0 && string(out) != string(data) {
			t.Errorf("round %d changed a stable file", i)
		}
		data = out
	}
}

// The scanner is hand written and runs over 22,000 files it did not choose.
// The invariant worth fuzzing is not what it returns but that it always
// returns: no panic, no unbounded loop, and never a src that would produce a
// broken opam file.
func FuzzParse(f *testing.F) {
	f.Add(wrappedSrc)
	f.Add(checksumList)
	f.Add(`url {`)
	f.Add(`url { src: "` + "\x00" + `" }`)
	f.Add(`description: """ url { src: "x" } `)
	f.Add(`extra-source "p" { src: "a" } url { src: "b" }`)

	f.Fuzz(func(t *testing.T, in string) {
		file, _ := Parse([]byte(in))
		if file.URL == nil {
			return // sourceless: nothing to rewrite
		}
		u := file.URL
		if u.Src == "" {
			t.Fatal("Parse returned a url source with an empty src")
		}
		if u.start < 0 || u.end > len(in) || u.start >= u.end {
			t.Fatalf("src range [%d,%d) is not inside a %d byte file", u.start, u.end, len(in))
		}

		// Rewriting must either refuse or produce a file that parses back to
		// exactly what we asked for.
		const replacement = "https://mirror.invalid/download/p/1.0/a.tar.gz"
		out, err := RewriteSrc([]byte(in), replacement)
		if err != nil {
			t.Fatalf("Parse found a url but RewriteSrc failed: %v", err)
		}
		again, _ := Parse(out)
		if again.URL == nil || again.URL.Src != replacement {
			t.Fatalf("round trip did not give back src %q", replacement)
		}
	})
}

const withExtraSources = `opam-version: "2.0"
url {
  src: "https://example.org/pkg-1.0.tar.gz"
  checksum: "sha256=aaaa"
}
extra-source "fix.patch" {
  src: "https://gist.example/raw/fix.patch"
  checksum: "md5=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
}
extra-source "second.patch" {
  src: "https://example.org/second.patch"
  checksum: "sha512=cccc"
}
`

func TestParseExtraSources(t *testing.T) {
	f, _ := Parse([]byte(withExtraSources))
	if f.URL == nil || f.URL.Src != "https://example.org/pkg-1.0.tar.gz" {
		t.Fatalf("url = %+v", f.URL)
	}
	if len(f.Extra) != 2 {
		t.Fatalf("got %d extra-sources, want 2", len(f.Extra))
	}
	if f.Extra[0].Name != "fix.patch" || f.Extra[0].Src != "https://gist.example/raw/fix.patch" {
		t.Errorf("extra[0] = %+v", f.Extra[0])
	}
	if !f.Extra[0].IsExtra() || f.URL.IsExtra() {
		t.Error("IsExtra is wrong")
	}
	if len(f.Sources()) != 3 {
		t.Errorf("Sources() = %d, want 3 (url + 2 extra)", len(f.Sources()))
	}
}

// The whole point of parsing extra-sources: opam fetches those patches at build
// time, so a mirror has to rewrite them too or they bypass it entirely.
func TestRewriteSourcesRewritesUrlAndExtras(t *testing.T) {
	f, _ := Parse([]byte(withExtraSources))
	out, err := RewriteSources([]byte(withExtraSources), f, func(s Source) string {
		if s.IsExtra() {
			return "https://mirror.internal/x/" + s.Name
		}
		return "https://mirror.internal/x/main.tar.gz"
	})
	if err != nil {
		t.Fatal(err)
	}

	g, _ := Parse(out)
	if g.URL.Src != "https://mirror.internal/x/main.tar.gz" {
		t.Errorf("url src = %q", g.URL.Src)
	}
	if len(g.Extra) != 2 ||
		g.Extra[0].Src != "https://mirror.internal/x/fix.patch" ||
		g.Extra[1].Src != "https://mirror.internal/x/second.patch" {
		t.Fatalf("extras not rewritten: %+v", g.Extra)
	}
	// Checksums must survive the rewrite, url and extra alike.
	if len(g.URL.Checksums) != 1 || g.Extra[0].Checksums[0].Kind != "md5" {
		t.Errorf("checksums changed: %+v / %+v", g.URL.Checksums, g.Extra[0].Checksums)
	}

	// A source whose replacement is empty is left exactly as it was.
	out2, _ := RewriteSources([]byte(withExtraSources), f, func(s Source) string {
		if s.IsExtra() {
			return ""
		}
		return "https://mirror.internal/only-main.tar.gz"
	})
	h, _ := Parse(out2)
	if h.Extra[0].Src != "https://gist.example/raw/fix.patch" {
		t.Errorf("extra changed despite empty replacement: %q", h.Extra[0].Src)
	}
	if h.URL.Src != "https://mirror.internal/only-main.tar.gz" {
		t.Errorf("url not rewritten: %q", h.URL.Src)
	}
}
