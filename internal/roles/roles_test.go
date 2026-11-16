package roles

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
)

func writeRole(t *testing.T, dir, name, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte(text), 0o644); err != nil {
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
			got, err := Find(dirs, tt.name)
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

func TestRender(t *testing.T) {
	r, err := Parse("r", "on {{.Branch}}{{if .PR}} spec {{.PR}}{{end}}")
	if err != nil {
		t.Fatal(err)
	}
	bad, err := Parse("bad", "{{.Nope}}")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		tmpl *template.Template
		data Data
		want string
	}{
		{name: "prompt phase is the task", tmpl: nil, data: Data{Task: "do it"}, want: "do it"},
		{name: "template fields", tmpl: r, data: Data{Branch: "b", PR: "/p.md"}, want: "on b spec /p.md"},
		{name: "task appended", tmpl: r, data: Data{Branch: "b", Task: "do it"}, want: "on b\n\n## Task\n\ndo it\n"},
		{name: "re-run with a report", tmpl: r, data: Data{Branch: "b", PriorRun: "abc", PriorReport: "/r.md"},
			want: "on b\n\n## This step ran before\n\nThis step ran before, ending at abc: read `git diff abc..HEAD` and its earlier report (/r.md) first, and read unchanged code only where you need it.\n"},
		{name: "re-run without one", tmpl: r, data: Data{Branch: "b", PriorRun: "abc"},
			want: "on b\n\n## This step ran before\n\nThis step ran before, ending at abc: read `git diff abc..HEAD` first, and read unchanged code only where you need it.\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Render(tt.tmpl, tt.data)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}

	if _, err := Render(bad, Data{}); err == nil {
		t.Error("render of unknown field: want error")
	}
}

// The built-in roles must render cleanly with every mix of PR and task, and
// mention each one exactly when it is given.
func TestBuiltinRolesRender(t *testing.T) {
	entries, err := fs.ReadDir(Builtin(), ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := fs.ReadFile(Builtin(), e.Name())
		if err != nil {
			t.Fatal(err)
		}
		name := strings.TrimSuffix(e.Name(), ".md")
		tmpl, err := Parse(name, string(b))
		if err != nil {
			t.Fatal(err)
		}
		for _, data := range []Data{
			{Branch: "feat/x", PR: "/plans/spec.md"},
			{Branch: "feat/x", Task: "TASK-TEXT"},
			{Branch: "feat/x", PR: "/plans/spec.md", Task: "TASK-TEXT"},
			{Branch: "feat/x"},
		} {
			if name == "implement" && data.PR == "" && data.Task == "" {
				continue // a job refuses implement with neither
			}
			got, err := Render(tmpl, data)
			if err != nil {
				t.Fatalf("%s %+v: %v", name, data, err)
			}
			if !strings.Contains(got, "feat/x") {
				t.Errorf("%s %+v: branch missing", name, data)
			}
			if strings.Contains(got, "/plans/spec.md") != (data.PR != "") {
				t.Errorf("%s %+v: PR mentioned = %v", name, data, data.PR == "")
			}
			if strings.Contains(got, "TASK-TEXT") != (data.Task != "") {
				t.Errorf("%s %+v: task mentioned = %v", name, data, data.Task == "")
			}
		}
	}
}
