// Package opamfile parses and rewrites the url section of an opam package
// description.
//
// It deliberately implements only as much of the opam file format as a mirror
// needs: locating the top-level "url" section, reading its "src" and
// "checksum" fields, and replacing the value of "src" without disturbing a
// single other byte of the file.
//
// Everything else in the file is opaque, and that is on purpose. A mirror that
// re-serialises opam files it does not fully understand will eventually corrupt
// one; a mirror that splices a single string literal cannot.
package opamfile

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
)

// ErrNoURL reports that a file carries no usable url.src. This is a normal
// condition, not a failure: conf-* packages and virtual packages have no
// source archive at all.
var ErrNoURL = errors.New("opamfile: no url.src")

// Checksum is one integrity digest declared by a package.
type Checksum struct {
	Kind string // "md5", "sha256" or "sha512"
	Hex  string
}

func (c Checksum) String() string { return c.Kind + "=" + c.Hex }

// URL is the content of a package's url section.
type URL struct {
	Src       string
	Checksums []Checksum

	// Byte range of the src string literal, quotes included, within the
	// file it was parsed from. RewriteSrc splices exactly this range.
	srcStart, srcEnd int
}

// Parse extracts the url section of an opam file. It returns ErrNoURL if the
// file declares no source archive.
func Parse(data []byte) (*URL, error) {
	bodyStart, bodyEnd, ok := findSection(data, "url")
	if !ok {
		return nil, ErrNoURL
	}

	u := &URL{}
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
			lit, start, end, ok := s.readString()
			if !ok {
				continue
			}
			u.Src, u.srcStart, u.srcEnd = lit, start, end
		case "checksum":
			u.Checksums = append(u.Checksums, s.readChecksums()...)
		default:
			s.skipValue()
		}
	}

	if u.Src == "" {
		return nil, ErrNoURL
	}
	return u, nil
}

// RewriteSrc returns a copy of data whose url.src is newSrc. Every other byte
// is preserved exactly.
//
// newSrc is rejected if it contains a quote, a backslash or a newline. A URL
// carrying a newline is the single most likely way to silently produce an opam
// repository that parses but resolves to nothing, so it is refused here rather
// than written out and discovered by a build three weeks later.
func RewriteSrc(data []byte, newSrc string) ([]byte, error) {
	u, err := Parse(data)
	if err != nil {
		return nil, err
	}
	if newSrc == "" {
		return nil, errors.New("opamfile: empty src")
	}
	if i := strings.IndexAny(newSrc, "\"\\\n\r\t"); i >= 0 {
		return nil, fmt.Errorf("opamfile: refusing to write src containing %q: %q", newSrc[i], newSrc)
	}

	out := make([]byte, 0, len(data)+len(newSrc))
	out = append(out, data[:u.srcStart]...)
	out = append(out, '"')
	out = append(out, newSrc...)
	out = append(out, '"')
	out = append(out, data[u.srcEnd:]...)
	return out, nil
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

// findSection returns the byte range of the body of the top-level section with
// the given name, braces excluded.
//
// Walking the top level rather than searching for "url {" matters: opam files
// also carry `extra-source "foo" { src: ... checksum: ... }` sections, which
// look identical to a regex and are not the package source.
func findSection(data []byte, name string) (start, end int, ok bool) {
	s := &scanner{data: data}
	for {
		s.skipTrivia()
		if s.eof() {
			return 0, 0, false
		}

		ident := s.readIdent()
		if ident == "" {
			s.pos++
			continue
		}
		s.skipTrivia()
		if s.eof() {
			return 0, 0, false
		}

		switch s.data[s.pos] {
		case ':': // field
			s.pos++
			s.skipValue()
		case '{': // section
			open := s.pos
			s.skipBalanced('{', '}')
			if ident == name {
				return open + 1, s.pos - 1, true
			}
		case '"': // labelled section: extra-source "foo" { ... }
			s.readString()
			s.skipTrivia()
			if !s.eof() && s.data[s.pos] == '{' {
				s.skipBalanced('{', '}')
			}
		default:
			s.pos++
		}
	}
}
