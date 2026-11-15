package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	actool "github.com/Autumn-27/norma/tool"

	"github.com/Autumn-27/artex/internal/oast"
)

// oastToolSet builds a ToolSet wired to a live HTTP-only callback channel.
func oastToolSet(t *testing.T) *ToolSet {
	t.Helper()
	srv := oast.New(oast.Options{HTTPAddr: "127.0.0.1:0"})
	if err := srv.Start(); err != nil {
		t.Fatalf("start oast: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ts := NewToolSet(nil, "worker")
	ts.SetOAST(srv)
	return ts
}

func findTool(t *testing.T, ts *ToolSet, name string) actool.CoreTool {
	t.Helper()
	for _, tl := range ts.OASTTools() {
		if tl.Name() == name {
			return tl
		}
	}
	t.Fatalf("%s not present", name)
	return nil
}

// fieldFromOutput pulls "key: value" out of the tool's text answer.
func fieldFromOutput(body, key string) string {
	for _, line := range strings.Split(body, "\n") {
		if v, ok := strings.CutPrefix(line, key+": "); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func TestOASTToolsAbsentWithoutChannel(t *testing.T) {
	ts := NewToolSet(nil, "worker")
	if got := ts.OASTTools(); got != nil {
		t.Fatalf("OASTTools = %d tools, want nil when no channel is configured", len(got))
	}
	// The worker set must not grow either — an unused tool still costs tokens.
	for _, tl := range ts.WorkerTools() {
		if strings.HasPrefix(tl.Name(), "oast_") {
			t.Fatalf("worker offered %s without a channel", tl.Name())
		}
	}
}

func TestOASTToolsPresentWithChannel(t *testing.T) {
	ts := oastToolSet(t)
	names := map[string]bool{}
	for _, tl := range ts.WorkerTools() {
		names[tl.Name()] = true
	}
	if !names["oast_new_url"] || !names["oast_hits"] {
		t.Fatalf("worker tools missing oast_*: %v", names)
	}
}

func TestOASTMintThenHit(t *testing.T) {
	ts := oastToolSet(t)
	mint := findTool(t, ts, "oast_new_url")

	res, err := mint.Call(context.Background(), json.RawMessage(`{"note":"SSRF on /fetch?url="}`), nil)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if res.IsError {
		t.Fatalf("mint returned a tool error: %s", res.Flatten())
	}
	body := res.Flatten()
	token := fieldFromOutput(body, "token")
	url := fieldFromOutput(body, "http_url")
	if token == "" || url == "" {
		t.Fatalf("mint output missing token or url: %s", body)
	}

	hits := findTool(t, ts, "oast_hits")

	// Before any callback: say so plainly instead of implying the payload failed.
	res, err = hits.Call(context.Background(), json.RawMessage(`{"token":"`+token+`"}`), nil)
	if err != nil {
		t.Fatalf("hits: %v", err)
	}
	if !strings.Contains(res.Flatten(), "no callback yet") {
		t.Fatalf("empty channel should say so plainly: %s", res.Flatten())
	}

	// The target reaches the callback URL.
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	_ = resp.Body.Close()

	// The listener runs in a goroutine; poll like the model would with wait.
	var out string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r, err := hits.Call(context.Background(), json.RawMessage(`{"token":"`+token+`"}`), nil)
		if err != nil {
			t.Fatalf("hits: %v", err)
		}
		out = r.Flatten()
		if strings.Contains(out, "callback(s)") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(out, "http from") {
		t.Fatalf("real callback not reported with proto and source: %s", out)
	}
	if !strings.Contains(out, "probe note: SSRF on /fetch?url=") {
		t.Fatalf("probe note lost, a hit cannot be attributed: %s", out)
	}
}

func TestOASTHitsRequiresToken(t *testing.T) {
	ts := oastToolSet(t)
	res, err := findTool(t, ts, "oast_hits").Call(context.Background(), json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.IsError {
		t.Fatalf("missing token must be a tool error, got %s", res.Flatten())
	}
}
