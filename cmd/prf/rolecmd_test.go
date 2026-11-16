package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newHome returns a prf home with the built-in roles installed.
func newHome(t *testing.T) string {
	t.Helper()
	h := filepath.Join(t.TempDir(), ".prf")
	if code := runPrf(t, "--home", h, "config", "init"); code != 0 {
		t.Fatalf("config init: exit %d", code)
	}
	return h
}

func prf(t *testing.T, h, stdin string, args ...string) (int, string) {
	t.Helper()
	return runPrfIO(t, stdin, append([]string{"--home", h}, args...)...)
}

func roleListJSON(t *testing.T, h string, args ...string) map[string]roleEntry {
	t.Helper()
	code, out := prf(t, h, "", append([]string{"role", "list", "--json"}, args...)...)
	if code != 0 {
		t.Fatalf("role list: exit %d", code)
	}
	var entries []roleEntry
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		t.Fatalf("role list --json: %v\n%s", err, out)
	}
	byName := map[string]roleEntry{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	return byName
}

func TestRoleList(t *testing.T) {
	h := newHome(t)
	writeRole(t, filepath.Join(h, "app", "roles"), "review", "# App review\n{{.Branch}}")
	writeRole(t, filepath.Join(h, "roles"), "broken", "{{.Nope}}")

	shared := roleListJSON(t, h)
	if len(shared) != 7 || shared["review"].Scope != "shared" || shared["review"].Description != "Idiomatic Go Reviewer (AI slop hunter)" {
		t.Errorf("shared roles = %+v", shared)
	}
	if b := shared["broken"]; b.Valid || b.Builtin || !strings.Contains(b.Error, "Nope") {
		t.Errorf("broken = %+v", b)
	}

	app := roleListJSON(t, h, "--repo", "app")
	review := app["review"]
	if review.Scope != "repo" || review.Description != "App review" || review.Overrides != filepath.Join(h, "roles", "review.md") || !review.Valid {
		t.Errorf("app review = %+v", review)
	}
	if app["test"].Scope != "shared" || app["review"].Content != "" {
		t.Errorf("app test = %+v", app["test"])
	}

	_, text := prf(t, h, "", "role", "list", "--repo", "app")
	if !strings.Contains(text, "INVALID") || !strings.Contains(text, "(overrides ") {
		t.Errorf("text list missing status:\n%s", text)
	}
}

func TestRoleShow(t *testing.T) {
	h := newHome(t)
	writeRole(t, filepath.Join(h, "app", "roles"), "perf", "perf on {{.Branch}}\n")

	if code, out := prf(t, h, "", "role", "show", "perf", "--repo", "app"); code != 0 || out != "perf on {{.Branch}}\n" {
		t.Errorf("show = %d %q", code, out)
	}
	code, out := prf(t, h, "", "role", "show", "perf", "--repo", "app", "--json")
	var e roleEntry
	if code != 0 || json.Unmarshal([]byte(out), &e) != nil || e.Content != "perf on {{.Branch}}\n" || e.Scope != "repo" {
		t.Errorf("show --json = %d %s", code, out)
	}
	for _, args := range [][]string{{"perf"}, {}, {"a", "b"}, {"../x"}, {"test", "--repo", "../x"}} {
		if code, _ := prf(t, h, "", append([]string{"role", "show"}, args...)...); code != exitUsage {
			t.Errorf("show %v: exit %d, want %d", args, code, exitUsage)
		}
	}
}

func TestRoleSet(t *testing.T) {
	h := newHome(t)
	read := func(path string) string {
		b, _ := os.ReadFile(path)
		return string(b)
	}

	if code, _ := prf(t, h, "perf on {{.Branch}}", "role", "set", "perf"); code != 0 {
		t.Fatalf("set shared: exit %d", code)
	}
	if got := read(filepath.Join(h, "roles", "perf.md")); got != "perf on {{.Branch}}" {
		t.Errorf("shared perf = %q", got)
	}

	f := filepath.Join(t.TempDir(), "r.md")
	writeFileT(t, f, "app perf")
	if code, _ := prf(t, h, "", "role", "set", "perf", "--repo", "app", "--file", f); code != 0 {
		t.Fatalf("set repo: exit %d", code)
	}
	if got := read(filepath.Join(h, "app", "roles", "perf.md")); got != "app perf" {
		t.Errorf("app perf = %q", got)
	}
	if got := read(filepath.Join(h, "roles", "perf.md")); got != "perf on {{.Branch}}" {
		t.Errorf("shared perf changed: %q", got)
	}

	for _, tt := range []struct{ name, stdin, role string }{
		{"bad syntax", "{{.Branch", "review"},
		{"unknown field", "{{.Nope}}", "review"},
		{"empty", "  \n", "review"},
		{"empty without a task", "{{.Task}}", "review"},
		{"bad name", "x", "a b"},
	} {
		t.Run("rejects "+tt.name, func(t *testing.T) {
			before := read(filepath.Join(h, "roles", "review.md"))
			if code, _ := prf(t, h, tt.stdin, "role", "set", tt.role); code != exitUsage {
				t.Errorf("exit %d, want %d", code, exitUsage)
			}
			if read(filepath.Join(h, "roles", "review.md")) != before {
				t.Error("rejected role still changed the file")
			}
		})
	}
	if entries, _ := os.ReadDir(filepath.Join(h, "roles")); len(entries) != 7 {
		t.Errorf("shared roles dir has leftovers: %v", entries)
	}
}

func TestRoleRemoveAndReset(t *testing.T) {
	h := newHome(t)
	writeRole(t, filepath.Join(h, "app", "roles"), "review", "app review")
	writeRole(t, filepath.Join(h, "roles"), "review", "edited review")

	code, out := prf(t, h, "", "role", "remove", "review", "--repo", "app")
	if code != 0 || !strings.Contains(out, "now resolves to "+filepath.Join(h, "roles", "review.md")) {
		t.Fatalf("remove = %d %q", code, out)
	}
	if code, _ := prf(t, h, "", "role", "remove", "review", "--repo", "app"); code != exitUsage {
		t.Errorf("removing a missing role: exit %d", code)
	}

	if code, _ := prf(t, h, "", "role", "reset", "review"); code != 0 {
		t.Fatalf("reset: exit %d", code)
	}
	builtin, _ := builtinRoles.ReadFile("roles/review.md")
	if b, _ := os.ReadFile(filepath.Join(h, "roles", "review.md")); string(b) != string(builtin) {
		t.Error("reset did not restore the built-in text")
	}
	if code, _ := prf(t, h, "", "role", "reset", "perf"); code != exitUsage {
		t.Errorf("reset of a non-built-in: exit %d", code)
	}
}

func TestFirstLine(t *testing.T) {
	for in, want := range map[string]string{
		"### Test Engineer\nmore":  "Test Engineer",
		"\n\n  plain first line\n": "plain first line",
		"":                         "",
	} {
		if got := firstLine(in); got != want {
			t.Errorf("firstLine(%q) = %q, want %q", in, got, want)
		}
	}
}
