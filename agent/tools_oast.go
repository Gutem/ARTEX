package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	actool "github.com/Autumn-27/norma/tool"

	"github.com/Autumn-27/artex/internal/oast"
)

// Out-of-band tools. They exist for the vulnerabilities that never answer:
// blind SSRF, blind command injection, blind XXE and JNDI lookups. Without a
// channel that the target can call back to, the only possible conclusion is
// "maybe", and this platform forbids registering a finding it did not trigger.
//
// Flow the prompt teaches: mint a token, inject its URL or hostname into the
// payload, wait a few seconds, then read the hits. A hit is proof the target made
// the request; the source address and timestamp go into the finding's evidence.

// oastProbe remembers one token so oast_hits can report what it was for. Kept in
// memory: a probe outlives a single tool call but not a process restart, which is
// the right lifetime for a callback channel.
type oastProbe struct {
	Token oast.Token
	Note  string
	At    time.Time
}

// OASTTools returns the two out-of-band tools, or nil when no channel is running
// so they cost no tokens in the prompt.
func (t *ToolSet) OASTTools() []actool.CoreTool {
	if t.oast == nil {
		return nil
	}
	return []actool.CoreTool{t.oastNewURL(), t.oastHits()}
}

// SetOAST attaches a callback channel to this tool set.
func (t *ToolSet) SetOAST(s *oast.Server) { t.oast = s }

func (t *ToolSet) oastNewURL() actool.CoreTool {
	return readTool("oast_new_url",
		"Get an out-of-band callback URL and hostname for a blind vulnerability. Inject one of them into the payload "+
			"(a URL parameter, a shell command, an XML entity, a JNDI string) and then call oast_hits with the returned id. "+
			"A recorded hit proves the target actually reached the channel — the only evidence available when nothing echoes back. "+
			"Mint a new token per probe so a hit can be attributed to one injection point.",
		obj(map[string]any{
			"note": str("What this probe is for, e.g. 'SSRF on /fetch?url=' (optional, appears in oast_hits)"),
		}),
		func(_ context.Context, args json.RawMessage) (actool.Result, error) {
			var in struct {
				Note string `json:"note"`
			}
			_ = json.Unmarshal(args, &in)

			tok := t.oast.NewToken()
			if tok.ID == "" {
				return actool.Errorf("no out-of-band listener is running; OAST must be configured on the server"), nil
			}
			t.oastMu.Lock()
			if t.oastProbes == nil {
				t.oastProbes = map[string]oastProbe{}
			}
			t.oastProbes[tok.ID] = oastProbe{Token: tok, Note: strings.TrimSpace(in.Note), At: time.Now()}
			t.oastMu.Unlock()

			return actool.Text(fmt.Sprintf("token: %s\nhttp_url: %s\ndns_host: %s\n\nInject one of them into the payload, then call oast_hits with token=%s.",
				tok.ID, tok.HTTPURL, tok.DNSHost, tok.ID)), nil
		})
}

func (t *ToolSet) oastHits() actool.CoreTool {
	return readTool("oast_hits",
		"Read the callbacks recorded for an out-of-band token. A hit carries the source address and the timestamp, "+
			"which is what you paste into evidence: it proves the target executed the payload. No hit is not proof of "+
			"absence — check that egress from the target is possible and that enough time passed before concluding.",
		obj(map[string]any{
			"token": str("Token id returned by oast_new_url"),
			"wait":  intp("Seconds to wait before reading (default 0, max 30)"),
		}),
		func(ctx context.Context, args json.RawMessage) (actool.Result, error) {
			var in struct {
				Token string `json:"token"`
				Wait  int    `json:"wait"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return actool.Errorf("invalid arguments: " + err.Error()), nil
			}
			id := strings.TrimSpace(in.Token)
			if id == "" {
				return actool.Errorf("token is required"), nil
			}
			if in.Wait > 0 {
				if in.Wait > 30 {
					in.Wait = 30
				}
				select {
				case <-time.After(time.Duration(in.Wait) * time.Second):
				case <-ctx.Done():
					return actool.Errorf("cancelled while waiting for callbacks"), nil
				}
			}

			hits := t.oast.Hits(id)
			if len(hits) == 0 {
				return actool.Text(fmt.Sprintf("token %s: no callback yet.\nBefore concluding the payload failed, consider: the target may have no egress, the DNS name may not be delegated, or it simply has not fired yet (wait a few more seconds).", id)), nil
			}

			var b strings.Builder
			fmt.Fprintf(&b, "token %s: %d callback(s)\n", id, len(hits))
			t.oastMu.RLock()
			if p, ok := t.oastProbes[id]; ok && p.Note != "" {
				fmt.Fprintf(&b, "probe note: %s\n", p.Note)
			}
			t.oastMu.RUnlock()
			for i, h := range hits {
				fmt.Fprintf(&b, "%d. %s from %s at %s — %s\n", i+1, h.Proto, h.Source, h.At.UTC().Format(time.RFC3339), h.Detail)
			}
			b.WriteString("\nA hit proves the target executed the payload. Record it with report_finding and quote the source address and timestamp in evidence.")
			return actool.Text(b.String()), nil
		})
}
