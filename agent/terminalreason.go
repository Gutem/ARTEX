package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Autumn-27/norma/harness"
)

// runTrace retains the latest tool call so an interrupted run can identify the
// operation that was still in flight.
type runTrace struct {
	startedAt time.Time
	id        string
	name      string
	input     string
	at        time.Time
	pending   bool
}

func (t *runTrace) start(id, name, input string) {
	t.id, t.name, t.input, t.at, t.pending = id, name, input, time.Now(), true
}

func (t *runTrace) done(id string) {
	if id == t.id {
		t.pending = false
	}
}

var reasonHint = map[harness.TerminalReason]string{
	harness.ReasonCompleted:         "the model finished this turn normally but left no written summary; the facts and assets stand as recorded by this turn's tool calls",
	harness.ReasonMaxTurns:          "step limit reached (MaxTurns): the SDK ran the wrap-up and wrote the facts and assets back, and the intent is marked exhausted so the planner can continue in another direction — this is not treated as a failure",
	harness.ReasonTimeout:           "the single-run wall-clock budget was reached (MaxDuration): the running tool is interrupted and wrap-up starts in place, writing back the facts and assets identified, and the intent is marked exhausted",
	harness.ReasonModelError:        "the model or API call failed (network, authentication, rate limiting, provider 5xx, etc.) and retries ran out, so the intent is marked blocked — a transport failure that means it was barely explored at all; inspect its execution trace (get_worker_trace) before deciding to re-dispatch or change approach",
	harness.ReasonBlockingLimit:     "the context length hit the hard limit and the request was blocked before being sent; narrow the intent granularity or compress the tool output",
	harness.ReasonPromptTooLong:     "the prompt is too long and the context-compaction retries are exhausted, so execution cannot continue",
	harness.ReasonImageError:        "the current model does not support this turn's multimodal content; switch to a vision-capable model or avoid tools that return images",
	harness.ReasonStopHookPrevented: "the Stop hook blocked the turn from ending and it could not continue afterwards; check whether the task's Guard rules are too strict",
	harness.ReasonHookStopped:       "a tool or hook deliberately stopped execution, for example an out-of-scope target or a forbidden command; check the block note in the last tool_result",
	harness.ReasonAbortedStreaming:  "the run was cancelled while the model was streaming its output",
	harness.ReasonAbortedTools:      "the run was cancelled while a tool was executing",
}

// terminalText renders a terminal event with no final text into a compact summary
// and a Markdown detail block.
func terminalText(ctx context.Context, term *harness.Terminal, tr *runTrace) (string, string) {
	reason := term.Reason
	aborted := reason == harness.ReasonAbortedStreaming || reason == harness.ReasonAbortedTools
	// Prompt may return ctx.Err directly without a terminal event. Preserve the
	// cancellation cause instead of falling back to an empty/unknown terminal reason.
	if reason == "" && ctx.Err() != nil {
		aborted = true
	}

	var sum string
	if aborted {
		_, short, _, ok := AbortReason(ctx)
		if !ok {
			short = "the cancellation cause could not be obtained"
		}
		stage := "during execution"
		switch reason {
		case harness.ReasonAbortedStreaming:
			stage = "model output stage"
		case harness.ReasonAbortedTools:
			stage = "tool execution stage"
		}
		sum = "(run interrupted: " + short + "; stopped at " + stage + progressSuffix(term, tr) + ", unfinished)"
	} else if reason == harness.ReasonMaxTurns || reason == harness.ReasonTimeout {
		sum = "(run budget limit reached (" + string(reason) + "), wrapped up and wrote back the facts" + progressSuffix(term, tr) + "; no written summary this time)"
	} else {
		hint := terminalReasonHint(reason)
		sum = "(no written summary, terminal state " + terminalReasonLabel(reason) + "：" + firstLine(hint, 80) + "）"
	}

	var b strings.Builder
	b.WriteString(sum)
	b.WriteString("\n\n")
	displayReason := terminalReasonLabel(reason)
	fmt.Fprintf(&b, "- **Terminal state**: `%s` - %s\n", displayReason, terminalReasonHint(reason))
	if aborted {
		code, _, why, ok := AbortReason(ctx)
		if ok {
			fmt.Fprintf(&b, "- **Interruption cause** (`%s`): %s\n", code, why)
		} else {
			b.WriteString("- **Interruption cause**: unavailable; the canceller may not have attached a named cause through context.WithCancelCause\n")
		}
	}
	if term.Err != nil {
		fmt.Fprintf(&b, "- **Underlying error**: `%v`\n", term.Err)
	}
	if aborted && strings.TrimSpace(term.Text) != "" {
		b.WriteString("- **Partial output produced before cancellation**:\n\n")
		b.WriteString(term.Text)
		b.WriteString("\n\n")
	}
	if term.Turns > 0 {
		fmt.Fprintf(&b, "- **Executed**: %d model turns\n", term.Turns)
	}
	if !tr.startedAt.IsZero() {
		fmt.Fprintf(&b, "- **Run duration**: %s\n", roundDur(time.Since(tr.startedAt)))
	}
	if u := term.Usage; u.InputTokens+u.OutputTokens+u.CacheReadTokens+u.CacheWriteTokens > 0 {
		fmt.Fprintf(&b, "- **Total tokens**: input %d / output %d / cache read %d / cache write %d\n",
			u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens)
	}
	if tr.name == "" {
		b.WriteString("- **Tool calls**: this run ended without issuing any tool call\n")
	} else if tr.pending {
		fmt.Fprintf(&b, "- **Tool executing at interruption**: `%s` (ran for %s, **returned no result**)\n\n  ```json\n  %s\n  ```\n",
			tr.name, roundDur(time.Since(tr.at)), firstLine(tr.input, 300))
	} else {
		fmt.Fprintf(&b, "- **Last tool before interruption**: `%s` (returned normally)\n", tr.name)
	}
	return sum, b.String()
}

func terminalReasonLabel(reason harness.TerminalReason) string {
	if reason == "" {
		return "context_canceled"
	}
	return string(reason)
}

func terminalReasonHint(reason harness.TerminalReason) string {
	if hint := reasonHint[reason]; hint != "" {
		return hint
	}
	if reason == "" {
		return "the run's context was cancelled but the underlying layer produced no Terminal event"
	}
	return "unknown terminal state; the harness may have added a TerminalReason, please add a reasonHint"
}

func progressSuffix(term *harness.Terminal, tr *runTrace) string {
	var parts []string
	if term.Turns > 0 {
		parts = append(parts, fmt.Sprintf("%d turns", term.Turns))
	}
	if !tr.startedAt.IsZero() {
		parts = append(parts, roundDur(time.Since(tr.startedAt)))
	}
	if len(parts) == 0 {
		return ""
	}
	return ", ran for " + strings.Join(parts, " / ")
}

func roundDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return d.Round(100 * time.Millisecond).String()
	case d < time.Hour:
		return d.Round(time.Second).String()
	default:
		return d.Round(time.Minute).String()
	}
}
