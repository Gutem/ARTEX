package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func intentNode(payload string) *db.Node {
	return &db.Node{Kind: db.KindIntent, State: "blocked", Payload: json.RawMessage(payload)}
}

func TestNodeStopReasonReadsKnownKeys(t *testing.T) {
	for _, key := range []string{"reason", "stop_reason", "terminal_reason", "error"} {
		n := intentNode(`{"summary":"probe WAF","` + key + `":"WAF rejected every payload"}`)
		if got := nodeStopReason(n); got != "WAF rejected every payload" {
			t.Errorf("key %q not read: %q", key, got)
		}
	}
}

func TestNodeStopReasonIgnoresFacts(t *testing.T) {
	// A fact carries a conclusion, not a decision to stop a direction; folding a
	// reason from a fact into the digest would invent a decision nobody made.
	n := &db.Node{Kind: db.KindFact, State: "open", Payload: json.RawMessage(`{"summary":"x","reason":"noise"}`)}
	if got := nodeStopReason(n); got != "" {
		t.Fatalf("fact produced a stop reason: %q", got)
	}
}

func TestNodeStopReasonEmptyWhenAbsent(t *testing.T) {
	n := intentNode(`{"summary":"probe"}`)
	if got := nodeStopReason(n); got != "" {
		t.Fatalf("absent reason returned %q", got)
	}
}

func TestCliplineCollapsesNewlinesAndClips(t *testing.T) {
	if got := clipLine("line one\nline two", 100); got != "line one line two" {
		t.Fatalf("newlines not collapsed: %q", got)
	}
	long := strings.Repeat("a", 300)
	if got := clipLine(long, 10); len(got) != 13 || !strings.HasSuffix(got, "…") {
		t.Fatalf("clip = %q", got)
	}
}
