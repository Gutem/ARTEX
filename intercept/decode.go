package intercept

// Payload decoding for the judge.
//
// The judge reads the tool call as the model wrote it. An encoded payload —
// base64, percent-encoding, \u escapes, HTML entities, hex — reads as noise to a
// reviewer while the shell decodes and runs it. That asymmetry is a bypass of the
// whole interception layer, and it does not require an attacker: agents encode
// routinely when a literal breaks a parser.
//
// These helpers expose every decodable view of the same string, so the judge sees
// what the target will actually receive. They never execute anything and never
// resolve a payload back towards output: a decoding failure is reported as such.

import (
	"encoding/base64"
	"encoding/hex"
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// DecodedView is one interpretation of a payload.
type DecodedView struct {
	Kind  string `json:"kind"`            // base64|url|hex|unicode|html|nested
	Text  string `json:"text"`            // the decoded form
	Depth int    `json:"depth,omitempty"` // how many decoding layers were applied
}

const (
	decodeMinLen  = 8    // shorter inputs decode to noise
	decodeMaxLen  = 4096 // beyond this the payload is a file, not an inline probe
	decodeMaxDeep = 3
)

var (
	base64Re = regexp.MustCompile(`^[A-Za-z0-9+/=_-]{8,}$`)
	hexRe    = regexp.MustCompile(`^(?:[0-9a-fA-F]{2}){4,}$`)
	uniRe    = regexp.MustCompile(`\\u[0-9a-fA-F]{4}`)
	uplusRe  = regexp.MustCompile(`%u[0-9a-fA-F]{4}`)
)

// DecodePayload returns every decoded view of s that differs from s itself.
// Duplicates are removed: the same plaintext reached by two encodings is one view.
func DecodePayload(s string) []DecodedView {
	s = strings.TrimSpace(s)
	if len(s) < decodeMinLen || len(s) > decodeMaxLen {
		return nil
	}
	seen := map[string]bool{s: true}
	var out []DecodedView
	add := func(kind, text string, depth int) {
		if text == "" || len(text) > decodeMaxLen*4 {
			return
		}
		if seen[text] {
			return
		}
		seen[text] = true
		out = append(out, DecodedView{Kind: kind, Text: text, Depth: depth})
	}

	add("url", urlDecode(s), 1)
	add("html", html.UnescapeString(s), 1)
	if v := unicodeUnescape(s); v != s {
		add("unicode", v, 1)
	}
	if hexRe.MatchString(s) {
		if b, err := hex.DecodeString(s); err == nil {
			add("hex", string(b), 1)
		}
	}
	if base64Re.MatchString(s) {
		for _, enc := range []*base64.Encoding{
			base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
		} {
			if b, err := enc.DecodeString(s); err == nil && len(b) > 0 && printable(string(b)) {
				add("base64", string(b), 1)
				break
			}
		}
	}

	// Nested: an encoded payload inside an encoded payload is common when a tool
	// wraps an argument in JSON before shelling out.
	for depth := 2; depth <= decodeMaxDeep; depth++ {
		layer := append([]DecodedView(nil), out...)
		for _, v := range layer {
			if v.Depth != depth-1 {
				continue
			}
			if d := urlDecode(v.Text); d != v.Text {
				add("url", d, depth)
			}
			if b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v.Text)); err == nil && len(b) > 0 && printable(string(b)) {
				add("base64", string(b), depth)
			}
		}
	}
	return out
}

// urlDecode decodes percent-encoding without touching '+' (which in a URL query
// means space, but in a payload is usually a literal plus).
func urlDecode(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	if v, err := url.PathUnescape(s); err == nil {
		return v
	}
	return s
}

// unicodeUnescape resolves \uXXXX and the legacy %uXXXX form.
func unicodeUnescape(s string) string {
	if !uniRe.MatchString(s) && !uplusRe.MatchString(s) {
		return s
	}
	repl := func(m string) string {
		hexPart := strings.TrimPrefix(strings.TrimPrefix(m, `\u`), "%u")
		if n, err := strconv.ParseUint(hexPart, 16, 32); err == nil {
			return string(rune(n))
		}
		return m
	}
	s = uniRe.ReplaceAllStringFunc(s, repl)
	return uplusRe.ReplaceAllStringFunc(s, repl)
}

// printable rejects decoded bytes that are obviously binary, so a random base64
// blob does not become a "decoded payload" the judge has to reason about.
func printable(s string) bool {
	if len(s) == 0 {
		return false
	}
	bad := 0
	for _, r := range s {
		if r < 0x20 && r != '\n' && r != '\t' && r != '\r' {
			bad++
		}
	}
	return bad*10 <= len(s)
}
