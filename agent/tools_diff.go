package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	actool "github.com/Autumn-27/norma/tool"
)

// Structural response diff.
//
// Why it exists: the three dimensions an agent hunts most often — authorization
// (IDOR), business logic and mass assignment — all reduce to the same action:
// compare two responses. Same object fetched by two identities; same request with
// one extra parameter; the same endpoint before and after a state change. Reading
// two bodies side by side is what a human does and what a model does badly, so the
// comparison is computed here and only the differences come back.

type httpDiffSpec struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
	Session string            `json:"session"`
}

func (t *ToolSet) httpDiffTool() actool.CoreTool {
	spec := func(label string) map[string]any {
		return obj(map[string]any{
			"method":  str(label + " HTTP method (default GET)"),
			"url":     str(label + " URL"),
			"headers": obj(map[string]any{"Name": str("Header value")}),
			"body":    str(label + " request body"),
			"session": str(label + " session name (cookies) — use two sessions to compare two identities"),
		}, "url")
	}
	return writeTool("http_diff",
		"Send TWO requests and return only the structural differences: status, length, which headers appeared/changed/vanished, and how the bodies differ (identical, prefix, or the first differing lines). "+
			"This is the cheapest way to decide authorization and logic findings: fetch the same object with session A and session B, or with and without one parameter, and read the diff instead of two raw bodies. "+
			"A body that is identical between two identities means no IDOR here; a 200 for the user who should not see it, with the same payload, is the finding.",
		obj(map[string]any{
			"a": spec("First"),
			"b": spec("Second"),
		}, "a", "b"),
		func(ctx context.Context, args json.RawMessage) (actool.Result, error) {
			var in struct {
				A httpDiffSpec `json:"a"`
				B httpDiffSpec `json:"b"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return actool.Errorf("invalid input: " + err.Error()), nil
			}
			ra, err := t.doHTTP(ctx, in.A.Session, "a", in.A.Method, in.A.URL, in.A.Headers, in.A.Body, false, 0, httpDefaultBodyCap)
			if err != nil {
				return actool.Errorf("request a failed: " + err.Error()), nil
			}
			rb, err := t.doHTTP(ctx, in.B.Session, "b", in.B.Method, in.B.URL, in.B.Headers, in.B.Body, false, 0, httpDefaultBodyCap)
			if err != nil {
				return actool.Errorf("request b failed: " + err.Error()), nil
			}
			return actool.Text(renderHTTPDiff(ra, rb)), nil
		})
}

// renderHTTPDiff writes the comparison. It deliberately states whether the bodies
// are identical: "no difference" is itself the answer for an IDOR probe, and a
// model reading two bodies tends to invent a difference that is not there.
func renderHTTPDiff(a, b *httpResponse) string {
	var s strings.Builder
	fmt.Fprintf(&s, "A: %s %s → %d, %d bytes\n", a.Method, a.URL, a.Status, len(a.Body))
	fmt.Fprintf(&s, "B: %s %s → %d, %d bytes\n", b.Method, b.URL, b.Status, len(b.Body))

	if a.Status != b.Status {
		fmt.Fprintf(&s, "\nstatus differs: %d vs %d\n", a.Status, b.Status)
	} else {
		fmt.Fprintf(&s, "\nstatus: same (%d)\n", a.Status)
	}

	// Header presence and value changes, limited to headers worth comparing.
	keys := map[string]bool{}
	for _, h := range []string{"Content-Type", "Content-Length", "Location", "Set-Cookie", "WWW-Authenticate", "Allow", "X-Frame-Options"} {
		keys[h] = true
	}
	for k := range a.Headers {
		keys[k] = true
	}
	for k := range b.Headers {
		keys[k] = true
	}
	var changed []string
	for k := range keys {
		va, vb := a.Headers.Get(k), b.Headers.Get(k)
		switch {
		case va == vb:
			continue
		case va == "":
			changed = append(changed, fmt.Sprintf("+ %s: %s", k, truncateHeader(vb)))
		case vb == "":
			changed = append(changed, fmt.Sprintf("- %s: %s", k, truncateHeader(va)))
		default:
			changed = append(changed, fmt.Sprintf("~ %s: %s → %s", k, truncateHeader(va), truncateHeader(vb)))
		}
	}
	sort.Strings(changed)
	if len(changed) == 0 {
		s.WriteString("headers: no relevant difference\n")
	} else {
		s.WriteString("headers:\n")
		for _, c := range changed {
			fmt.Fprintf(&s, "  %s\n", c)
		}
	}

	switch {
	case string(a.Body) == string(b.Body):
		s.WriteString("\nbody: IDENTICAL — the two requests returned the same bytes. For an authorization probe this means no difference, so do not claim one.\n")
	default:
		fmt.Fprintf(&s, "\nbody: differs (%d vs %d bytes)\n", len(a.Body), len(b.Body))
		for _, line := range firstDifferingLines(a.Body, b.Body, 6) {
			s.WriteString("  " + line + "\n")
		}
	}
	return s.String()
}

func truncateHeader(v string) string {
	if len(v) > 80 {
		return v[:80] + "…"
	}
	return v
}

// firstDifferingLines walks both bodies line by line and returns up to max lines
// from the first index where they diverge, prefixed with the side they came from.
func firstDifferingLines(a, b []byte, max int) []string {
	la := strings.Split(strings.ReplaceAll(string(a), "\r\n", "\n"), "\n")
	lb := strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
	n := len(la)
	if len(lb) < n {
		n = len(lb)
	}
	start := 0
	for start < n && strings.TrimSpace(la[start]) == strings.TrimSpace(lb[start]) {
		start++
	}
	var out []string
	for i := start; i < len(la) && len(out) < max; i++ {
		out = append(out, fmt.Sprintf("A %d: %s", i+1, clip(la[i], 160)))
	}
	for i := start; i < len(lb) && len(out) < max; i++ {
		out = append(out, fmt.Sprintf("B %d: %s", i+1, clip(lb[i], 160)))
	}
	if len(out) == 0 {
		out = append(out, "(bodies differ only in trailing whitespace)")
	}
	return out
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
