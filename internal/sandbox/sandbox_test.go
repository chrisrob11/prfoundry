package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/chrisrob11/prfoundry/internal/textfile"
)

func writeFileT(t *testing.T, path, s string) {
	t.Helper()
	if err := textfile.Write(path, s); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultSettings(t *testing.T) {
	var got claudeSettings
	if err := json.Unmarshal(DefaultSettings(), &got); err != nil {
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
	home := t.TempDir()
	global := filepath.Join(home, SettingsFile)
	var logged []string
	logf := func(format string, _ ...any) { logged = append(logged, format) }

	got, err := SettingsPath(home, "app", logf)
	if err != nil {
		t.Fatal(err)
	}
	if got != global {
		t.Fatalf("got %s, want the shared default %s", got, global)
	}
	if _, err := os.Stat(global); err != nil {
		t.Fatalf("default not created: %v", err)
	}
	if len(logged) != 1 {
		t.Errorf("logged %q, want the new default announced once", logged)
	}

	// An edited default is used as is, never rewritten.
	writeFileT(t, global, "{}")
	if got, _ := SettingsPath(home, "app", logf); got != global {
		t.Errorf("got %s, want %s", got, global)
	}
	if b, _ := os.ReadFile(global); string(b) != "{}" {
		t.Errorf("existing settings were overwritten: %q", b)
	}

	// A repo-level file wins for that repo only.
	repoLevel := filepath.Join(home, "app", SettingsFile)
	writeFileT(t, repoLevel, "{}")
	if got, _ := SettingsPath(home, "app", logf); got != repoLevel {
		t.Errorf("app: got %s, want %s", got, repoLevel)
	}
	if got, _ := SettingsPath(home, "other", logf); got != global {
		t.Errorf("other: got %s, want %s", got, global)
	}
}

func TestGoCacheDirsWithoutGo(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no go here
	t.Setenv("HOME", "/h")
	t.Setenv("XDG_CACHE_HOME", "/h/.cache") // used by os.UserCacheDir on Linux
	for _, k := range []string{"GOPATH", "GOCACHE"} {
		t.Setenv(k, "")
	}
	t.Setenv("GOMODCACHE", "/custom/mod")

	cacheDir, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/custom/mod", filepath.Join(cacheDir, "go-build"), "/h/go/pkg/sumdb"}
	if got := goCacheDirs(); !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
