package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestDefaultSettings(t *testing.T) {
	var got claudeSettings
	if err := json.Unmarshal(defaultSettings(), &got); err != nil {
		t.Fatalf("default settings are not valid JSON: %v", err)
	}
	sb := got.Sandbox
	if !sb.Enabled || !sb.FailIfUnavailable || sb.AllowUnsandboxedCommands || !sb.Network.StrictAllowlist {
		t.Errorf("sandbox is not locked down: %+v", sb)
	}
	var readRules int
	for _, rule := range got.Permissions.Deny {
		if strings.HasPrefix(rule, "Read(") {
			readRules++
		}
	}
	if len(sb.Filesystem.DenyRead) != readRules {
		t.Errorf("denyRead %v and Read deny rules %v should cover the same paths", sb.Filesystem.DenyRead, got.Permissions.Deny)
	}
	for _, rule := range []string{"Bash(git push:*)", "Bash(rtk git push:*)", "Bash(gh:*)", "Bash(rtk gh:*)"} {
		if !slices.Contains(got.Permissions.Deny, rule) {
			t.Errorf("Claude should be denied %s", rule)
		}
	}
}

func TestSettingsPath(t *testing.T) {
	home = t.TempDir()
	global := filepath.Join(home, settingsFile)

	got, err := settingsPath("app")
	if err != nil {
		t.Fatal(err)
	}
	if got != global {
		t.Fatalf("got %s, want the shared default %s", got, global)
	}
	if _, err := os.Stat(global); err != nil {
		t.Fatalf("default not created: %v", err)
	}

	// An edited default is used as is, never rewritten.
	writeFileT(t, global, "{}")
	if got, _ := settingsPath("app"); got != global {
		t.Errorf("got %s, want %s", got, global)
	}
	if b, _ := os.ReadFile(global); string(b) != "{}" {
		t.Errorf("existing settings were overwritten: %q", b)
	}

	// A repo-level file wins for that repo only.
	repoLevel := filepath.Join(home, "app", settingsFile)
	writeFileT(t, repoLevel, "{}")
	if got, _ := settingsPath("app"); got != repoLevel {
		t.Errorf("app: got %s, want %s", got, repoLevel)
	}
	if got, _ := settingsPath("other"); got != global {
		t.Errorf("other: got %s, want %s", got, global)
	}
}
