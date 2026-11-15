package agent

// This file records the historical built-in prompt bodies this fork has shipped,
// so an existing installation can be upgraded without touching operator edits.
//
// Why it is needed: SeedPromptIfEmpty writes the code default only when an agent
// has no prompt version yet. Every installation created before the English
// translation therefore stays on the Chinese body forever, and the only escape is
// the manual "restore built-in default" action. Upgrading by hash removes that
// dead end: the stored text is compared byte for byte against a version we know
// we shipped, and only an unedited built-in gets replaced.
//
// When a default prompt changes again, add the outgoing hash to legacyPromptHashes
// under the same agent key. Never remove an entry: old installations keep
// arriving from older releases.

// legacyPromptHashes lists, per agent key, the sha256 of every built-in body
// previously shipped. These are the pre-translation (Chinese, v0.3.30) versions.
var legacyPromptHashes = map[string][]string{
	"goals":        {"72a7a0159443b41dde29dfba8a3de6a2c8a15b273935495a7b88561fa708aa68"},
	"planner":      {"6cf6551450f6a62b1da33e0fd0eb7e20337803e87f8a6e2dbb251c9dbaa7da6e"},
	"mainagent":    {"12bb69ee0886c4fb9beb936d882021a91104860f1da69a5e3eda3d9e345cd399"},
	"worker":       {"80fbb1c610dd57e266c20b9145793339eee540b809f6db8f5336261b45879f08"},
	"auto":         {"38c454b6a6d353c8c833a70bad866a11869a70a239075c16377c14eb8a9cba2d"},
	"asset_mapper": {"2ec7172abec230597719b1465b6441dba03adb0d2f00ac9912a7df23877073ee"},
	"pentest":      {"a54bd5713b1c121d0f2ff3a72e0304820b1368c4b4b8159d5d651895663ac652"},
}

// PromptUpgrade is one pending migration of a built-in prompt body.
type PromptUpgrade struct {
	Key         string
	OldHash     string
	Replacement string
	Note        string
}

// PromptUpgrades returns the upgrades to attempt on startup: for each agent key,
// every historical hash is paired with the current built-in body. Which of them
// actually applies is decided against the database, per installation.
func PromptUpgrades() []PromptUpgrade {
	seeds := BuiltinPromptSeeds()
	var out []PromptUpgrade
	for key, hashes := range legacyPromptHashes {
		current, ok := seeds[key]
		if !ok || current == "" {
			continue
		}
		for _, h := range hashes {
			out = append(out, PromptUpgrade{
				Key:         key,
				OldHash:     h,
				Replacement: current,
				Note:        "built-in prompt upgraded (untouched versions only)",
			})
		}
	}
	return out
}
