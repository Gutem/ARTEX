package agent

import (
	"strings"
	"testing"
	"unicode"
)

// hasCJK reports whether s contains a CJK ideograph. Upstream ships these prompts
// in Chinese; this fork keeps them in English so the conduct an agent follows is
// auditable by an English-reading operator — and so a resync with upstream cannot
// silently reintroduce Chinese into a system prompt.
func hasCJK(s string) bool {
	for _, r := range s {
		if unicode.In(r, unicode.Han) {
			return true
		}
	}
	return false
}

// TestSystemPromptsAreEnglish pins every built-in system prompt to English.
//
// It will fail the moment a merge from upstream brings a Chinese default back.
// Fix it by translating, not by relaxing the assertion: prompts are behavior.
func TestSystemPromptsAreEnglish(t *testing.T) {
	cases := map[string]string{
		"opsec":               opsecPromptBlock,
		"compression":         compressionSystemPrompt,
		"autoTitle":           autoTitleSystemPrompt,
		"defaultAssistant":    DefaultAssistantPrompt,
		"reporter":            ReporterDefaultPrompt,
		"goalScopeTail":       goalsScopeTail,
		"plannerWrapUp":       plannerWrapUpDefault,
		"mainAgentWrapUp":     mainAgentWrapUpDefault,
		"genericWrapUp":       genericWrapUpDefault,
		"credentialProcessor": CredentialProcessorPrompt,
		"retester":            RetesterDefaultPrompt,
		"workerTrafficBlock":  workerTrafficBlock(true),
		"workerArtifactSpec":  workerArtifactSpec("/tmp/run"),
		"taskArtifactSpec":    artifactSpec("/tmp/task"),
	}
	for key, tmpl := range BuiltinPromptSeeds() {
		cases["seed:"+key] = tmpl
	}

	for name, text := range cases {
		if text == "" {
			t.Errorf("%s: prompt is empty", name)
			continue
		}
		if hasCJK(text) {
			line := firstCJKLine(text)
			t.Errorf("%s: system prompt still contains Chinese: %q", name, line)
		}
	}
}

// firstCJKLine returns the first line of s that carries a CJK ideograph, so a
// failure points at the offending text instead of dumping a multi-KB prompt.
func firstCJKLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if hasCJK(line) {
			if len(line) > 120 {
				return line[:120] + "…"
			}
			return line
		}
	}
	return ""
}
