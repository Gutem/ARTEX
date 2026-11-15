package db

import (
	"testing"
)

// upgradeFixture creates an agent with a prompt version holding body, emulating an
// installation seeded before a default changed.
func upgradeFixture(t *testing.T, d *DB, body string) int64 {
	t.Helper()
	a, err := d.CreateAgent("test_upgrade_"+PromptTemplateHash(body)[:8], "Test Upgrade", "")
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	agentID := a.ID
	t.Cleanup(func() {
		d.Exec(`DELETE FROM agent_prompts WHERE agent_id=$1`, agentID)
		d.Exec(`DELETE FROM agents WHERE id=$1`, agentID)
	})
	if _, err := d.SavePrompt(agentID, body, "built-in default", "system"); err != nil {
		t.Fatalf("save prompt: %v", err)
	}
	return agentID
}

func currentPrompt(t *testing.T, d *DB, agentID int64) string {
	t.Helper()
	var body string
	if err := d.QueryRow(`
		SELECT COALESCE(p.template_text,'') FROM agents a
		LEFT JOIN agent_prompts p ON p.id=a.current_prompt_id WHERE a.id=$1`, agentID).Scan(&body); err != nil {
		t.Fatalf("read current prompt: %v", err)
	}
	return body
}

func TestUpgradeDefaultPromptHashReplacesUntouchedBody(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	old := "你是一个授权渗透测试系统的\"独立渗透 agent\"。"
	replacement := "You are the solo pentest agent of an authorized platform."
	agentID := upgradeFixture(t, d, old)

	changed, err := d.UpgradeDefaultPromptHash(agentID, PromptTemplateHash(old), replacement, "upgrade test")
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if !changed {
		t.Fatal("expected the untouched built-in body to be replaced")
	}
	if got := currentPrompt(t, d, agentID); got != replacement {
		t.Fatalf("current prompt = %q, want the replacement", got)
	}
}

func TestUpgradeDefaultPromptHashKeepsOperatorEdits(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	builtin := "你是一个授权渗透测试系统的\"独立渗透 agent\"。"
	edited := builtin + "\n\n- operator added this line"
	agentID := upgradeFixture(t, d, edited)

	changed, err := d.UpgradeDefaultPromptHash(agentID, PromptTemplateHash(builtin), "English replacement", "upgrade test")
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if changed {
		t.Fatal("an edited prompt must not be replaced")
	}
	if got := currentPrompt(t, d, agentID); got != edited {
		t.Fatalf("operator edit lost: %q", got)
	}
}

func TestUpgradeDefaultPromptHashIsIdempotent(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	old := "旧的内置提示词。"
	replacement := "The new built-in body."
	agentID := upgradeFixture(t, d, old)
	hash := PromptTemplateHash(old)

	if ok, err := d.UpgradeDefaultPromptHash(agentID, hash, replacement, "first"); err != nil || !ok {
		t.Fatalf("first upgrade: ok=%v err=%v", ok, err)
	}
	ok, err := d.UpgradeDefaultPromptHash(agentID, hash, replacement, "second")
	if err != nil {
		t.Fatalf("second upgrade: %v", err)
	}
	if ok {
		t.Fatal("second run must be a no-op once the body already matches")
	}
	var versions int
	if err := d.QueryRow(`SELECT count(*) FROM agent_prompts WHERE agent_id=$1`, agentID).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 2 {
		t.Fatalf("prompt versions = %d, want 2 (original + one upgrade)", versions)
	}
}
