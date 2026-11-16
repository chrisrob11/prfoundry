package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/chrisrob11/prfoundry/internal/textfile"
)

func writeRole(t *testing.T, dir, name, text string) {
	t.Helper()
	if err := textfile.Write(filepath.Join(dir, name+".md"), text); err != nil {
		t.Fatal(err)
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
			phases, err := loadPhases(roleSteps(tt.roles...), dirs, tt.hasWork, tt.hasTask)
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
