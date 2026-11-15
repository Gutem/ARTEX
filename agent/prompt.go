package agent

import (
	"bytes"
	"text/template"
	"time"
)

// PromptOverride, if set, returns the stored system-prompt template for an agent
// key and whether one exists. The server wires it to the PG agent_prompts table.
// When nil or no override exists, agents use their built-in default prompt — so
// behavior is identical until a user edits a prompt in the UI.
var PromptOverride func(agentKey string) (string, bool)

// OpsecEnabled is wired by the server to the persisted global setting. Keeping
// the block here makes it a code-owned runtime tail: database-edited prompts
// cannot accidentally remove the operational safety guidance.
var OpsecEnabled func() bool

const opsecPromptBlock = `[OPSEC RULES — hard constraints, second only to the authorized scope and to the task's operation constraints]

- Reuse existing traffic, browser sessions and already-verified evidence first; when traffic_search/traffic_get or the browser can answer it, do not repeat the request directly.
- Every interaction with the target must go through the platform-configured recording proxy or the browser tools, so the request stays traceable.
- Do not use bare curl, urllib, requests or similar with default headers against the target. Prefer reusing the User-Agent, Accept, Referer and Cookie characteristics already present in the target's traffic; the platform HTTP tools and collection templates take their default UA from the system's randomized policy, and redirects and retries inside one request must keep the same UA; an explicitly configured UA takes precedence.
- Do not use -k, --ignore-https-errors, verify=False or similar to bypass certificate validation; when the platform proxy CA is configured, validate normally.
- Do not bulk or concurrent request, loop rapidly, retry without limit, sweep ports, brute-force directories or run large-scale scans; on rate limiting, captcha, WAF or an anomalous response, stop the current direction immediately and do not resume it by switching UA.
- OPSEC does not expand the authorized scope; when the target, host, port, path or account is outside the authorized scope, do not access it.
- Whether a request and its response are recorded is the platform proxy's and the browser's job; do not additionally copy full request headers, cookies or response bodies.`

// Prompt-variable structs — fields mirror each agent's catalog (docs §5a) so a
// user template referencing a catalog variable renders; referencing anything else
// fails template execution and falls back to the built-in default.
type PlannerVars struct{ Goal, Scope, AssetSummary, DataDir, Now string }
type WorkerVars struct{ ProxyAddr, WorkerName, DataDir, Now string }
type MainVars struct{ Goal, AssetSummary, FindingsSummary, DataDir, Now string }
type GoalsVars struct{ EngagementDescription, DataDir, Now string }

// nowStr is the server-local wall-clock string exposed as the universal {{.Now}}
// prompt variable. renderSystem runs on every agent turn/round, so this is fresh
// each run — a prompt can subtract it from a fixed start stamp to reason about
// elapsed time (e.g. a timed benchmark's "last N hours" window).
func nowStr() string { return time.Now().Format("2006-01-02 15:04:05 MST") }

// renderSystem returns the rendered system-prompt BODY (段 [A]) for agentKey.
// Precedence: the DB-stored template (if any) over the built-in default template
// (def). BOTH are Go templates now — the built-in default is seeded into the DB
// verbatim, so the two paths render identically until a user edits the prompt.
// Rendering always runs (def used to be pre-substituted plain text; it is now a
// {{.Var}} template like the DB one). On any render error we fall back to the
// default template, then to the raw default string — an agent never starts with a
// half-rendered prompt. Callers append the code-owned tail (trafficTool / 中间产物
// 输出规约) AFTER this, so those can't be edited away via the DB body.
func renderSystem(agentKey, def string, vars any) string {
	tmpl := def
	if PromptOverride != nil {
		if t, ok := PromptOverride(agentKey); ok && t != "" {
			tmpl = t
		}
	}
	if out, err := renderTmpl(tmpl, vars); err == nil {
		if OpsecEnabled != nil && OpsecEnabled() {
			return out + "\n\n" + opsecPromptBlock
		}
		return out
	}
	// DB template broke (e.g. references an out-of-catalog var) → code default.
	if out, err := renderTmpl(def, vars); err == nil {
		if OpsecEnabled != nil && OpsecEnabled() {
			return out + "\n\n" + opsecPromptBlock
		}
		return out
	}
	if OpsecEnabled != nil && OpsecEnabled() {
		return def + "\n\n" + opsecPromptBlock
	}
	return def
}

func renderTmpl(tmpl string, vars any) (string, error) {
	t, err := template.New("p").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, vars); err != nil {
		return "", err
	}
	return b.String(), nil
}
