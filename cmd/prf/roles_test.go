package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRole(t *testing.T, dir, name, text string) {
	t.Helper()
	if err := writeFile(filepath.Join(dir, name+".md"), text); err != nil {
		t.Fatal(err)
	}
}

func TestFindRole(t *testing.T) {
	local, global := t.TempDir(), t.TempDir()
	writeRole(t, local, "review", "local review")
	writeRole(t, global, "review", "global review")
	writeRole(t, global, "test", "global test")
	dirs := []string{local, global}

	tests := []struct {
		name    string
		want    string
		wantErr string
	}{
		{name: "review", want: "local review"},
		{name: "test", want: "global test"},
		{name: "missing", wantErr: `no role "missing"`},
		{name: "../review", wantErr: "invalid role name"},
		{name: "a b", wantErr: "invalid role name"},
		{name: "a/b", wantErr: "invalid role name"},
		{name: "", wantErr: "invalid role name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := findRole(dirs, tt.name)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLoadPhases(t *testing.T) {
	dir := t.TempDir()
	writeRole(t, dir, "implement", "impl {{.Branch}}")
	writeRole(t, dir, "review", "review {{.Branch}}")
	writeRole(t, dir, "broken", "{{.Branch")
	dirs := []string{dir}

	tests := []struct {
		name      string
		roles     []string
		hasWork   bool
		hasTask   bool
		wantNames []string
		wantErr   string
	}{
		{name: "roles in order", roles: []string{"review", "implement"}, hasWork: true, wantNames: []string{"review", "implement"}},
		{name: "task only", hasWork: true, hasTask: true, wantNames: []string{"prompt"}},
		{name: "nothing to do", wantErr: "give --prompt, --pipeline, --role, or a mix"},
		{name: "implement without work", roles: []string{"implement"}, wantErr: "needs --pr or --prompt"},
		{name: "review without work", roles: []string{"review"}, wantNames: []string{"review"}},
		{name: "bad template", roles: []string{"broken"}, hasWork: true, wantErr: "role broken"},
		{name: "unknown role", roles: []string{"nope"}, hasWork: true, wantErr: `no role "nope"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			phases, err := loadPhases(roles(tt.roles...), dirs, tt.hasWork, tt.hasTask)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, p := range phases {
				names = append(names, p.name)
			}
			if strings.Join(names, ",") != strings.Join(tt.wantNames, ",") {
				t.Errorf("phases = %v, want %v", names, tt.wantNames)
			}
		})
	}
}

func TestRender(t *testing.T) {
	dir := t.TempDir()
	writeRole(t, dir, "r", "on {{.Branch}}{{if .PR}} spec {{.PR}}{{end}}")
	writeRole(t, dir, "bad", "{{.Nope}}")
	phases, err := loadPhases(roles("r", "bad"), []string{dir}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	r, bad := phases[0], phases[1]

	tests := []struct {
		name  string
		phase phase
		data  promptData
		want  string
	}{
		{name: "prompt phase is the task", phase: phase{name: "prompt"}, data: promptData{Task: "do it"}, want: "do it"},
		{name: "template fields", phase: r, data: promptData{Branch: "b", PR: "/p.md"}, want: "on b spec /p.md"},
		{name: "task appended", phase: r, data: promptData{Branch: "b", Task: "do it"}, want: "on b\n\n## Task\n\ndo it\n"},
		{name: "re-run with a report", phase: r, data: promptData{Branch: "b", PriorRun: "abc", PriorReport: "/r.md"},
			want: "on b\n\n## This step ran before\n\nThis step ran before, ending at abc: read `git diff abc..HEAD` and its earlier report (/r.md) first, and read unchanged code only where you need it.\n"},
		{name: "re-run without one", phase: r, data: promptData{Branch: "b", PriorRun: "abc"},
			want: "on b\n\n## This step ran before\n\nThis step ran before, ending at abc: read `git diff abc..HEAD` first, and read unchanged code only where you need it.\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.phase.render(tt.data)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}

	if _, err := bad.render(promptData{}); err == nil {
		t.Error("render of unknown field: want error")
	}
}

// The built-in roles must render cleanly with every mix of PR and task, and
// mention each one exactly when it is given.
func TestBuiltinRolesRender(t *testing.T) {
	dir := t.TempDir()
	entries, err := builtinRoles.ReadDir("roles")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		b, err := builtinRoles.ReadFile("roles/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), b, 0o644); err != nil {
			t.Fatal(err)
		}
		names = append(names, strings.TrimSuffix(e.Name(), ".md"))
	}
	phases, err := loadPhases(roles(names...), []string{dir}, true, true)
	if err != nil {
		t.Fatal(err)
	}

	for _, p := range phases {
		for _, data := range []promptData{
			{Branch: "feat/x", PR: "/plans/spec.md"},
			{Branch: "feat/x", Task: "TASK-TEXT"},
			{Branch: "feat/x", PR: "/plans/spec.md", Task: "TASK-TEXT"},
			{Branch: "feat/x"},
		} {
			if p.name == "implement" && data.PR == "" && data.Task == "" {
				continue // rejected by loadPhases
			}
			got, err := p.render(data)
			if err != nil {
				t.Fatalf("%s %+v: %v", p.name, data, err)
			}
			if !strings.Contains(got, "feat/x") {
				t.Errorf("%s %+v: branch missing", p.name, data)
			}
			if strings.Contains(got, "/plans/spec.md") != (data.PR != "") {
				t.Errorf("%s %+v: PR mentioned = %v", p.name, data, data.PR == "")
			}
			if strings.Contains(got, "TASK-TEXT") != (data.Task != "") {
				t.Errorf("%s %+v: task mentioned = %v", p.name, data, data.Task == "")
			}
		}
	}
}
