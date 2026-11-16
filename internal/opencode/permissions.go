package opencode

import (
	"fmt"
	"path/filepath"
)

// rule is one of opencode's permission rules. The last rule matching an
// action wins; an action no rule matches asks, and a headless run rejects
// what asks, unless --auto approves it.
type rule struct {
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Effect   string `json:"effect"`
}

func allow(actions ...string) []rule {
	rules := make([]rule, len(actions))
	for i, a := range actions {
		rules[i] = rule{a, "*", "allow"}
	}
	return rules
}

// modeRules maps a Claude Code permission mode to opencode's rules, and
// whether the run approves whatever they don't decide (--auto).
func modeRules(mode string) ([]rule, bool, error) {
	reads := allow("read", "glob", "grep")
	switch mode {
	case "default", "dontAsk": // only what's allowed; anything else is refused
		return reads, false, nil
	case "acceptEdits":
		return append(reads, allow("edit")...), false, nil
	case "plan":
		return append(reads, rule{"edit", "*", "deny"}, rule{"shell", "*", "deny"}), false, nil
	case "bypassPermissions":
		return nil, true, nil
	}
	return nil, false, fmt.Errorf("opencode can't run in permission mode %q; use default, dontAsk, acceptEdits, plan or bypassPermissions", mode)
}

// fixedDenials hold in every mode, after the mode's rules so they win: no
// pushing or gh (prf ships the branch itself), and no writing opencode's
// own config, which the next step would load.
var fixedDenials = []rule{
	{"shell", "git push *", "deny"},
	{"shell", "rtk git push *", "deny"},
	{"shell", "gh *", "deny"},
	{"shell", "rtk gh *", "deny"},
	{"edit", "*opencode.json", "deny"},
	{"edit", "*opencode.jsonc", "deny"},
	{"edit", "*.opencode/*", "deny"},
}

// permissions are a run's rules: the mode's, the folders outside the
// checkout it may use, then fixedDenials.
func permissions(mode []rule, dirs []string) []rule {
	rules := append([]rule{}, mode...)
	for _, d := range dirs {
		rules = append(rules, rule{"external_directory", d, "allow"}, rule{"external_directory", filepath.Join(d, "*"), "allow"})
	}
	return append(rules, fixedDenials...)
}
