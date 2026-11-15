package db

import (
	"strings"
	"testing"
)

func TestHardRequirementTermsDropsGenericWords(t *testing.T) {
	terms := HardRequirementTerms("Do not run nmap against the target, passive only — no brute force")
	have := map[string]bool{}
	for _, tm := range terms {
		have[tm] = true
	}
	for _, want := range []string{"nmap", "brute", "force"} {
		if !have[want] {
			t.Errorf("missing checkable term %q (got %v)", want, terms)
		}
	}
	for _, banned := range []string{"the", "not", "only", "target"} {
		if have[banned] {
			t.Errorf("generic word %q must not become a match term", banned)
		}
	}
}

func TestHardRequirementTermsHandlesPortugueseConstraint(t *testing.T) {
	terms := HardRequirementTerms("Não escanear portas; permitido apenas verificar o painel administrativo")
	have := map[string]bool{}
	for _, tm := range terms {
		have[tm] = true
	}
	if !have["escanear"] || !have["portas"] {
		t.Fatalf("Portuguese constraint lost its terms: %v", terms)
	}
}

// hardFixture builds a task with an exploration, a deny constraint and one
// recorded action, so the gate can be exercised end to end.
func hardFixture(t *testing.T, d *DB, constraint, actionSummary string) int64 {
	t.Helper()
	var expID, taskID int64
	if err := d.QueryRow(`INSERT INTO explorations(goal) VALUES ('hard-requirement-fixture') RETURNING id`).Scan(&expID); err != nil {
		t.Fatalf("create exploration: %v", err)
	}
	if err := d.QueryRow(`INSERT INTO tasks(name, description, goal, status, exploration_id) VALUES ('gate fixture','fixture','prove the gate','running',$1) RETURNING id`, expID).Scan(&taskID); err != nil {
		t.Fatalf("create task: %v", err)
	}
	t.Cleanup(func() {
		d.Exec(`DELETE FROM activity WHERE exploration_id=$1`, expID)
		d.Exec(`DELETE FROM task_constraints WHERE exploration_id=$1`, expID)
		d.Exec(`DELETE FROM tasks WHERE id=$1`, taskID)
		d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	})
	if _, err := d.Exec(`INSERT INTO task_constraints(exploration_id, kind, text) VALUES ($1,'deny',$2)`, expID, constraint); err != nil {
		t.Fatalf("create constraint: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO activity(exploration_id, kind, tool, summary) VALUES ($1,'tool_use','Bash',$2)`, expID, actionSummary); err != nil {
		t.Fatalf("create activity: %v", err)
	}
	return taskID
}

func TestCompletionBlockedWhenDenyConstraintMatchesAction(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	taskID := hardFixture(t, d, "Do not run nmap at all", "ran nmap -sS 10.0.0.0/24")

	msg, err := d.CompletionBlockedByHardRequirement(taskID, "")
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	if !strings.Contains(msg, "completion blocked") {
		t.Fatalf("gate should refuse completion, got %q", msg)
	}
	if !strings.Contains(msg, "nmap") {
		t.Fatalf("refusal must name the matched term: %q", msg)
	}
}

func TestCompletionAllowedWhenNothingMatches(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	taskID := hardFixture(t, d, "Do not run nmap at all", "read /api/orders with session alice")

	msg, err := d.CompletionBlockedByHardRequirement(taskID, "")
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	if msg != "" {
		t.Fatalf("gate should pass, got %q", msg)
	}
}

func TestCompletionOverrideBypassesGate(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	taskID := hardFixture(t, d, "Do not run nmap at all", "ran nmap -sS 10.0.0.0/24")

	msg, err := d.CompletionBlockedByHardRequirement(taskID, "the operator authorized this scan in writing")
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	if msg != "" {
		t.Fatalf("an explicit override must let completion through, got %q", msg)
	}
}

func TestCompletionAllowedWithoutConstraints(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	var expID, taskID int64
	if err := d.QueryRow(`INSERT INTO explorations(goal) VALUES ('hard-no-constraint') RETURNING id`).Scan(&expID); err != nil {
		t.Fatalf("create exploration: %v", err)
	}
	if err := d.QueryRow(`INSERT INTO tasks(name, description, goal, status, exploration_id) VALUES ('no constraint','fixture','prove the gate','running',$1) RETURNING id`, expID).Scan(&taskID); err != nil {
		t.Fatalf("create task: %v", err)
	}
	t.Cleanup(func() {
		d.Exec(`DELETE FROM tasks WHERE id=$1`, taskID)
		d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	})

	msg, err := d.CompletionBlockedByHardRequirement(taskID, "")
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	if msg != "" {
		t.Fatalf("no constraint means no gate, got %q", msg)
	}
}
