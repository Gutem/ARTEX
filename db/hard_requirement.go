package db

// Hard-requirement gate at task completion.
//
// The interception layer judges one tool call at a time. Nobody looks at the SET
// of actions a task executed before it declares itself done — so a task can honor
// every individual rule and still end up having done something the operator
// forbade in the brief ("no port scans", "passive only", "do not touch the
// production database").
//
// This gate closes that gap deterministically: for every deny constraint that
// names a concrete term, it looks for that term in the task's recorded actions and
// refuses completion when one matches. It deliberately does not try to read intent
// — a semantic judgment belongs to a model, and a model in the completion path
// makes the outcome unreproducible. What is checked here is literal and
// auditable, which is the property an engagement needs.
//
// Limits, stated so nobody trusts it further than it goes: a constraint written
// in prose without a checkable term ("be careful") never matches, and a command
// can express a forbidden action without containing the term. The override exists
// for exactly that, and bypassing the gate leaves a trail.

import (
	"fmt"
	"regexp"
	"strings"
)

// HardRequirementViolation is one deny constraint matched against one action.
type HardRequirementViolation struct {
	ConstraintID   int64  `json:"constraint_id"`
	ConstraintText string `json:"constraint_text"`
	Term           string `json:"term"`
	ActivityKind   string `json:"activity_kind"`
	ActivityText   string `json:"activity_text"`
	ActivityAt     string `json:"activity_at"`
}

// hardTermStopWords are words that appear in almost every constraint and would
// match any command, so they never become a match term.
var hardTermStopWords = map[string]bool{
	"the": true, "and": true, "not": true, "any": true, "all": true, "only": true,
	"with": true, "from": true, "this": true, "that": true, "must": true, "never": true,
	"does": true, "for": true, "use": true, "used": true, "into": true, "against": true,
	"target": true, "targets": true, "task": true, "allowed": true, "forbidden": true,
	"operation": true, "operations": true, "test": true, "tests": true, "scan": true,
	"para": true, "por": true, "com": true, "que": true, "uma": true, "dos": true,
	"das": true, "sem": true, "nao": true, "não": true,
}

var hardTermRe = regexp.MustCompile(`[A-Za-z0-9_][A-Za-z0-9_.\-/@]{2,}`)

// The schema keys constraints and activity by exploration, not by task: a task
// owns exactly one exploration (tasks.exploration_id), and both tables hang off
// it. These helpers resolve the exploration from the task id so callers can keep
// speaking in task ids.

// taskExplorationID returns the exploration that a task owns, or 0 when the task
// has none (a task created outside the exploration flow).
func (d *DB) taskExplorationID(taskID int64) (int64, error) {
	var expID int64
	err := d.QueryRow(`SELECT COALESCE(exploration_id,0) FROM tasks WHERE id=$1`, taskID).Scan(&expID)
	if err != nil {
		return 0, err
	}
	return expID, nil
}

// HardRequirementTerms extracts the checkable terms of a constraint: the words
// specific enough that meeting one in an action is evidence, not coincidence.
func HardRequirementTerms(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range hardTermRe.FindAllString(text, -1) {
		t := strings.ToLower(strings.Trim(m, ".-/@"))
		if len(t) < 3 || hardTermStopWords[t] || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// HardRequirementViolations reads the task's deny constraints and matches their
// terms against the actions the task recorded. Matching is a case-insensitive
// substring test on the action summary and detail.
func (d *DB) HardRequirementViolations(taskID int64) ([]HardRequirementViolation, error) {
	expID, err := d.taskExplorationID(taskID)
	if err != nil {
		return nil, err
	}
	if expID == 0 {
		return nil, nil
	}

	rows, err := d.Query(`
		SELECT id, text FROM task_constraints
		WHERE exploration_id=$1 AND kind='deny'
		ORDER BY id`, expID)
	if err != nil {
		return nil, err
	}
	type constraint struct {
		id   int64
		text string
	}
	var cs []constraint
	for rows.Next() {
		var c constraint
		if err := rows.Scan(&c.id, &c.text); err != nil {
			rows.Close()
			return nil, err
		}
		cs = append(cs, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(cs) == 0 {
		return nil, nil
	}

	// The gate runs at completion, so the recent window is the relevant one and
	// keeps the check cheap regardless of how long the task ran.
	acts, err := d.Query(`
		SELECT COALESCE(kind,''), COALESCE(tool,''), COALESCE(summary,''), COALESCE(detail,''), created_at::text
		FROM activity
		WHERE exploration_id=$1
		ORDER BY id DESC
		LIMIT 500`, expID)
	if err != nil {
		return nil, err
	}
	type action struct{ kind, tool, summary, detail, at string }
	var list []action
	for acts.Next() {
		var a action
		if err := acts.Scan(&a.kind, &a.tool, &a.summary, &a.detail, &a.at); err != nil {
			acts.Close()
			return nil, err
		}
		list = append(list, a)
	}
	if err := acts.Err(); err != nil {
		acts.Close()
		return nil, err
	}
	acts.Close()

	var out []HardRequirementViolation
	for _, c := range cs {
		for _, term := range HardRequirementTerms(c.text) {
			for _, a := range list {
				haystack := strings.ToLower(a.tool + " " + a.summary + " " + a.detail)
				if !strings.Contains(haystack, term) {
					continue
				}
				out = append(out, HardRequirementViolation{
					ConstraintID:   c.id,
					ConstraintText: c.text,
					Term:           term,
					ActivityKind:   a.kind,
					ActivityText:   clipText(a.summary+" "+a.detail, 240),
					ActivityAt:     a.at,
				})
				break // one match per term is enough to refuse
			}
		}
	}
	return out, nil
}

// CompletionBlockedByHardRequirement reports the refusal message for a task that
// cannot be completed, or "" when the gate passes. A non-empty override bypasses
// the gate; the caller is responsible for recording it.
func (d *DB) CompletionBlockedByHardRequirement(taskID int64, override string) (string, error) {
	if strings.TrimSpace(override) != "" {
		return "", nil
	}
	vs, err := d.HardRequirementViolations(taskID)
	if err != nil {
		return "", err
	}
	if len(vs) == 0 {
		return "", nil
	}
	var b strings.Builder
	b.WriteString("completion blocked: the task executed actions that its own deny constraints forbid.\n")
	for _, v := range vs {
		fmt.Fprintf(&b, "- constraint #%d %q matched %q in %s: %s\n", v.ConstraintID, v.ConstraintText, v.Term, v.ActivityKind, v.ActivityText)
	}
	b.WriteString("\nReview the actions. To close the task anyway, retry with an override reason, which is recorded for audit.")
	return b.String(), nil
}

func clipText(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
