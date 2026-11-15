package db

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
)

// UpgradeDefaultPromptHash replaces an agent's current prompt with replacement
// only when the stored text is byte-identical to a previously shipped built-in
// version (identified by oldHash).
//
// Background: SeedPromptIfEmpty is first-insert only, so an installation that
// already has a prompt version never receives an improved default. Rewriting
// unconditionally would silently discard the operator's edits, which is worse.
// Matching on the hash draws the line exactly where it belongs: whoever never
// edited gets the new built-in text, whoever edited keeps it.
//
// The agent row is locked (FOR UPDATE OF a) so the hash comparison and the
// current-version switch cannot interleave with a concurrent edit.
func (d *DB) UpgradeDefaultPromptHash(agentID int64, oldHash, replacement, note string) (bool, error) {
	if replacement == "" {
		return false, errors.New("replacement prompt is empty")
	}
	tx, err := d.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var current string
	err = tx.QueryRow(`
		SELECT COALESCE(p.template_text, '')
		FROM agents a
		LEFT JOIN agent_prompts p ON p.id = a.current_prompt_id
		WHERE a.id = $1
		FOR UPDATE OF a`, agentID).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if current == "" || current == replacement {
		return false, nil
	}
	if fmt.Sprintf("%x", sha256.Sum256([]byte(current))) != oldHash {
		return false, nil
	}

	var version int
	if err = tx.QueryRow(`SELECT COALESCE(max(version),0)+1 FROM agent_prompts WHERE agent_id=$1`, agentID).Scan(&version); err != nil {
		return false, err
	}
	var promptID int64
	if err = tx.QueryRow(
		`INSERT INTO agent_prompts(agent_id,version,template_text,note,updated_by) VALUES($1,$2,$3,$4,'system') RETURNING id`,
		agentID, version, replacement, note,
	).Scan(&promptID); err != nil {
		return false, err
	}
	if _, err = tx.Exec(`UPDATE agents SET current_prompt_id=$2 WHERE id=$1`, agentID, promptID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// PromptTemplateHash is the sha256 a built-in prompt body had when it was seeded.
// Callers compare stored text against a known list of historical hashes.
func PromptTemplateHash(text string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(text)))
}
