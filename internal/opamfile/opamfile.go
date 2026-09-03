// Package opamfile parses and rewrites the downloadable sources of an opam
// package description.
//
// It deliberately implements only as much of the opam file format as a mirror
// needs: locating the top-level "url" section and every "extra-source" section,
// reading their "src" and "checksum" fields, and replacing the value of each
// "src" without disturbing a single other byte of the file.
//
// Everything else in the file is opaque, and that is on purpose. A mirror that
// re-serialises opam files it does not fully understand will eventually corrupt
// one; a mirror that splices a string literal in place cannot.
//
// Parse never fails: a file it cannot make sense of simply yields no sources,
// and a mirror leaves such a file untouched rather than crashing on it. One
// malformed package upstream must not take down a rebuild of twenty thousand.
package opamfile

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"bytes"
)

// ErrNoURL reports that a file carries no usable url.src. conf-* packages and
// virtual packages have no source archive at all, which is normal.
var ErrNoURL = errors.New("opamfile: no url.src")

// Checksum is one integrity digest declared by a package.
type Checksum struct {
	Kind string // "md5", "sha256" or "sha512"
	Hex  string
}

func (c Checksum) String() string { return c.Kind + "=" + c.Hex }

// Source is one downloadable archive a package refers to: its url section, or
// one of its extra-source sections. opam fetches extra-sources (patches and the
// like) separately at build time, so a mirror that ignores them leaves a hole
// through which builds still reach the public internet.
type Source struct {
	Name      string // "" for the url section; the label for an extra-source
	Src       string
	Checksums []Checksum

	// Byte range of the src string literal, quotes included, in the file it
	// was parsed from.
	start, end int
}

// IsExtra reports whether this is an extra-source rather than the url section.
func (s Source) IsExtra() bool { return s.Name != "" }

// File is the set of sources declared by an opam file.
type File struct {
	URL   *Source  // the package's own source archive; nil for conf/virtual packages
	Extra []Source // extra-source sections, in file order
}

// Sources returns the url section (if any) followed by every extra-source.
func (f *File) Sources() []Source {
	out := make([]Source, 0, 1+len(f.Extra))
	if f.URL != nil {
		out = append(out, *f.URL)
	}
	return append(out, f.Extra...)
}

// Parse extracts every source of an opam file. It never returns an error; a
// file with no url section yields f.URL == nil.
func Parse(data []byte) (*File, error) {
	f := &File{}
	walkSections(data, func(name, label string, bodyStart, bodyEnd int) {
		switch {
		case name == "url":
			if src, ok := parseSource(data, bodyStart, bodyEnd, ""); ok && f.URL == nil {
				f.URL = &src
			}
		case name == "extra-source" && label != "":
			if src, ok := parseSource(data, bodyStart, bodyEnd, label); ok {
				f.Extra = append(f.Extra, src)
			}
		}
	})
	return f, nil
}

// parseSource reads the src and checksum fields inside one section body.
func parseSource(data []byte, bodyStart, bodyEnd int, label string) (Source, bool) {
	src := Source{Name: label}
	s := &scanner{data: data[:bodyEnd], pos: bodyStart}
	for {
		s.skipTrivia()
		if s.eof() {
			break
		}
		name := s.readIdent()
		if name == "" {
			s.pos++ // no progress possible; do not spin
			continue
		}
		s.skipTrivia()
		if s.eof() || s.data[s.pos] != ':' {
			continue
		}
		s.pos++
		s.skipTrivia()

		switch name {
		case "src", "archive":
			if lit, start, end, ok := s.readString(); ok {
				src.Src, src.start, src.end = lit, start, end
			}
		case "checksum":
			src.Checksums = append(src.Checksums, s.readChecksums()...)
		default:
			s.skipValue()
		}
	}
	if src.Src == "" {
		return Source{}, false
	}
	return src, true
}

// RewriteSources returns a copy of data where the src of each source is replaced
// by newSrc(source). A source for which newSrc returns "" is left untouched.
// Every other byte of the file is preserved exactly.
//
// A replacement is rejected if it contains a quote, a backslash or a control
// character. Such a value is the single most likely way to silently produce an
// opam repository that parses but resolves to nothing, so it is refused here
// rather than written out and discovered by a build three weeks later.
func RewriteSources(data []byte, f *File, newSrc func(Source) string) ([]byte, error) {
	type repl struct {
		start, end int
		val        string
	}
	var repls []repl

	add := func(s Source) error {
		v := newSrc(s)
		if v == "" {
			return nil
		}
		if i := strings.IndexAny(v, "\"\\\n\r\t"); i >= 0 {
			return fmt.Errorf("opamfile: refusing to write src containing %q: %q", v[i], v)
		}
		repls = append(repls, repl{s.start, s.end, v})
		return nil
	}
	if f.URL != nil {
		if err := add(*f.URL); err != nil {
			return nil, err
		}
	}
	for _, e := range f.Extra {
		if err := add(e); err != nil {
			return nil, err
		}
	}

	if len(repls) == 0 {
		return append([]byte(nil), data...), nil
	}
	sort.Slice(repls, func(i, j int) bool { return repls[i].start < repls[j].start })

	out := make([]byte, 0, len(data)+64)
	prev := 0
	for _, r := range repls {
		out = append(out, data[prev:r.start]...)
		out = append(out, '"')
		out = append(out, r.val...)
		out = append(out, '"')
		prev = r.end
	}
	return append(out, data[prev:]...), nil
}

// RewriteSrc rewrites only the url section's src, leaving extra-sources alone.
// It is a convenience over RewriteSources for callers that touch the main
// source only.
func RewriteSrc(data []byte, newSrc string) ([]byte, error) {
	f, err := Parse(data)
	if err != nil {
		return nil, err
	}
	if f.URL == nil {
		return nil, ErrNoURL
	}
	if newSrc == "" {
		return nil, errors.New("opamfile: empty src")
	}
	return RewriteSources(data, f, func(s Source) string {
		if s.IsExtra() {
			return ""
		}
		return newSrc
	})
}

// parseChecksum reads the "kind=hex" form opam uses.
func parseChecksum(s string) (Checksum, bool) {
	kind, hex, found := strings.Cut(strings.TrimSpace(s), "=")
	if !found {
		return Checksum{}, false
	}
	switch kind {
	case "md5", "sha256", "sha512":
	default:
		return Checksum{}, false
	}
	if hex == "" {
		return Checksum{}, false
	}
	return Checksum{Kind: kind, Hex: strings.ToLower(hex)}, true
}

// --- scanner ---------------------------------------------------------------

type scanner struct {
	data []byte
	pos  int
}

func (s *scanner) eof() bool { return s.pos >= len(s.data) }

// skipTrivia consumes whitespace and comments, newlines included: opam wraps
// long values onto the following line freely.
func (s *scanner) skipTrivia() {
	for !s.eof() {
		switch c := s.data[s.pos]; {
		case c == ' ', c == '\t', c == '\n', c == '\r':
			s.pos++
		case c == '#':
			for !s.eof() && s.data[s.pos] != '\n' {
				s.pos++
			}
		default:
			return
		}
	}
}

func (s *scanner) readIdent() string {
	start := s.pos
	for !s.eof() {
		c := s.data[s.pos]
		isWord := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
			c >= '0' && c <= '9' || c == '-' || c == '_' || c == '+'
		if !isWord {
			break
		}
		s.pos++
	}
	return string(s.data[start:s.pos])
}

// readString reads a string literal and returns its unescaped value together
// with the byte range it occupies, quotes included. It handles both "..." and
// the triple-quoted """...""" form used by description fields, which is what
// keeps brace counting honest: descriptions contain braces and quotes.
func (s *scanner) readString() (val string, start, end int, ok bool) {
	if s.eof() || s.data[s.pos] != '"' {
		return "", 0, 0, false
	}
	start = s.pos

	if bytes.HasPrefix(s.data[s.pos:], []byte(`"""`)) {
		s.pos += 3
		i := bytes.Index(s.data[s.pos:], []byte(`"""`))
		if i < 0 {
			s.pos = len(s.data)
			return "", 0, 0, false
		}
		val = string(s.data[s.pos : s.pos+i])
		s.pos += i + 3
		return val, start, s.pos, true
	}

	s.pos++ // opening quote
	var b strings.Builder
	for !s.eof() {
		c := s.data[s.pos]
		switch {
		case c == '\\' && s.pos+1 < len(s.data):
			s.pos++
			switch e := s.data[s.pos]; e {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			default:
				b.WriteByte(e)
			}
			s.pos++
		case c == '"':
			s.pos++
			return b.String(), start, s.pos, true
		default:
			b.WriteByte(c)
			s.pos++
		}
	}
	return "", 0, 0, false
}

// readChecksums reads either a single "kind=hex" literal or a list of them.
func (s *scanner) readChecksums() []Checksum {
	var out []Checksum
	s.skipTrivia()
	if s.eof() {
		return nil
	}

	if s.data[s.pos] != '[' {
		if lit, _, _, ok := s.readString(); ok {
			if c, ok := parseChecksum(lit); ok {
				out = append(out, c)
			}
		}
		return out
	}

	s.pos++ // '['
	for !s.eof() {
		s.skipTrivia()
		if s.eof() || s.data[s.pos] == ']' {
			s.pos++
			return out
		}
		lit, _, _, ok := s.readString()
		if !ok {
			s.pos++ // not a string; keep moving
			continue
		}
		if c, ok := parseChecksum(lit); ok {
			out = append(out, c)
		}
	}
	return out
}

// skipValue consumes one field value of any shape, plus a trailing filter.
func (s *scanner) skipValue() {
	s.skipTrivia()
	if s.eof() {
		return
	}
	switch s.data[s.pos] {
	case '"':
		s.readString()
	case '[':
		s.skipBalanced('[', ']')
	case '{':
		s.skipBalanced('{', '}')
	default:
		// A bare token: identifier, number, boolean, or a small expression
		// such as `os != "win32"`. These do not span lines.
		for !s.eof() && s.data[s.pos] != '\n' && s.data[s.pos] != '#' {
			s.pos++
		}
	}
	s.skipTrivia()
	if !s.eof() && s.data[s.pos] == '{' { // filter, e.g. {os = "linux"}
		s.skipBalanced('{', '}')
	}
}

// skipBalanced consumes a bracketed group, ignoring brackets that occur inside
// string literals or comments.
func (s *scanner) skipBalanced(open, close byte) {
	if s.eof() || s.data[s.pos] != open {
		return
	}
	depth := 0
	for !s.eof() {
		switch c := s.data[s.pos]; {
		case c == '"':
			s.readString()
			continue
		case c == '#':
			for !s.eof() && s.data[s.pos] != '\n' {
				s.pos++
			}
			continue
		case c == open:
			depth++
		case c == close:
			depth--
			if depth == 0 {
				s.pos++
				return
			}
		}
		s.pos++
	}
}

// walkSections calls fn for every top-level section, giving its name, its label
// (the quoted string for a labelled section like extra-source, "" otherwise),
// and the byte range of its body with the braces excluded.
//
// Walking the top level rather than searching for "url {" or "extra-source"
// matters: those tokens also appear inside string literals and descriptions,
// where a regex would find phantom sections.
func walkSections(data []byte, fn func(name, label string, bodyStart, bodyEnd int)) {
	s := &scanner{data: data}
	for {
		s.skipTrivia()
		if s.eof() {
			return
		}

		ident := s.readIdent()
		if ident == "" {
			s.pos++
			continue
		}
		s.skipTrivia()
		if s.eof() {
			return
		}

		switch s.data[s.pos] {
		case ':': // field
			s.pos++
			s.skipValue()
		case '{': // section
			open := s.pos
			s.skipBalanced('{', '}')
			fn(ident, "", open+1, s.pos-1)
		case '"': // labelled section: extra-source "foo" { ... }
			label, _, _, _ := s.readString()
			s.skipTrivia()
			if !s.eof() && s.data[s.pos] == '{' {
				open := s.pos
				s.skipBalanced('{', '}')
				fn(ident, label, open+1, s.pos-1)
			}
		default:
			s.pos++
		}
	}
}
