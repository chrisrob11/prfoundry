package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// expectEnv is an env with roles that commit, do nothing, leave untracked
// files, or both, and pipeline.json holding pipe (JSON object members).
func expectEnv(t *testing.T, pipe string) (env, *syncBuffer) {
	e := newEnv(t, "")
	logs := captureStderr(t)
	writeRole(t, e.roles, "commit", "COMMIT on {{.Branch}}")
	writeRole(t, e.roles, "plain", "plain on {{.Branch}}")
	writeRole(t, e.roles, "dirty", "DIRTY on {{.Branch}}")
	writeRole(t, e.roles, "both", "COMMIT DIRTY on {{.Branch}}")
	writeFileT(t, filepath.Join(e.home, "origin", pipelineFile), "{"+pipe+"}")
	return e, logs
}

func TestExpect(t *testing.T) {
	for _, tt := range []struct {
		name, pipe string
		want       int
		wantLog    string
	}{
		{"commits, and it did", `"pipelines": {"p": [{"role": "commit", "expect": "commits"}]}`, 0, ""},
		{"commits, and it made none", `"pipelines": {"p": [{"role": "plain", "expect": "commits"}]}`, exitExpect,
			"expected commits, but it made no commits"},
		{"commits, but it left changes too", `"pipelines": {"p": [{"role": "both", "expect": "commits"}]}`, exitExpect,
			"expected commits, but it left uncommitted changes: ?? dirty1.txt, ?? dirty2.txt, ?? dirty3.txt, ?? dirty4.txt, ?? dirty5.txt and 2 more"},
		{"read-only, and it was", `"pipelines": {"p": [{"role": "plain", "expect": "read-only"}]}`, 0, ""},
		{"read-only, but it committed", `"pipelines": {"p": [{"role": "commit", "expect": "read-only"}]}`, exitExpect,
			"expected read-only, but it made commits: HEAD moved from "},
		{"read-only, but it committed and left changes", `"pipelines": {"p": [{"role": "both", "expect": "read-only"}]}`, exitExpect,
			", and left uncommitted changes: ?? dirty1.txt"},
		{"clean, and it was", `"pipelines": {"p": [{"role": "commit", "expect": "clean"}]}`, 0, ""},
		{"clean, but it left changes", `"pipelines": {"p": [{"role": "dirty", "expect": "clean"}]}`, exitExpect,
			"expected clean, but it left uncommitted changes: ?? dirty1.txt"},
		{"the repo's expect is the default", `"expect": "commits", "pipelines": {"p": ["plain"]}`, exitExpect, "made no commits"},
		{"a step's own expect wins", `"expect": "commits", "pipelines": {"p": [{"role": "plain", "expect": "clean"}]}`, 0, ""},
		{"no expect, no check", `"pipelines": {"p": ["dirty"]}`, 0, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e, logs := expectEnv(t, `"default": "p", `+tt.pipe)
			if code := e.run(t, "run", "--repo", e.origin, "--branch", "b"); code != tt.want {
				t.Fatalf("exit %d, want %d\n%s", code, tt.want, logs.String())
			}
			if !strings.Contains(logs.String(), tt.wantLog) {
				t.Errorf("log missing %q:\n%s", tt.wantLog, logs.String())
			}
			status := readFile(t, filepath.Join(e.root, "work", "origin", "b", "status"))
			if failed := strings.Contains(status, "exit=10 error=false turns=3 cost=0.05 expect="); failed != (tt.want != 0) {
				t.Errorf("status:\n%s", status)
			}
		})
	}
}

// A step that breaks its expectation stops the job there, and resume runs it again.
func TestExpectResume(t *testing.T) {
	e, _ := expectEnv(t, `"default": "p", "pipelines": {"p": [{"role": "plain", "expect": "commits"}, "dirty"]}`)
	if code := e.run(t, "run", "--repo", e.origin, "--branch", "b"); code != exitExpect {
		t.Fatalf("run: exit %d, want %d", code, exitExpect)
	}
	jobDir := filepath.Join(e.root, "work", "origin", "b")
	if st := readState(t, jobDir); st.State != stateFailed || st.Step != 1 || st.Exit == nil || *st.Exit != exitExpect {
		t.Errorf("state = %+v", st)
	}
	if n := len(e.calls(t)); n != 1 {
		t.Errorf("ran %d steps, want 1", n)
	}

	writeRole(t, e.roles, "plain", "COMMIT on {{.Branch}}")
	if code := e.run(t, "resume", "origin/b"); code != 0 {
		t.Fatalf("resume: exit %d", code)
	}
	if status := readFile(t, filepath.Join(jobDir, "status")); !strings.Contains(status, "phase=plain step=1/2 exit=0") {
		t.Errorf("resume should run step 1 again:\n%s", status)
	}
}

// A role that stops to ask isn't held to its expectation; its re-run is.
func TestExpectWithAsk(t *testing.T) {
	e := askEnv(t, 1, `"@ask"`)
	writeFileT(t, filepath.Join(e.home, "origin", pipelineFile), `{"default": "a", "pipelines": {"a": [{"role": "asker", "expect": "commits"}, {"gate": "ask", "question": ".agent/stop.md"}, "test"]}}`)
	fakeOwner(t, answer)
	if code := e.run(t, "run", "--repo", e.origin, "--branch", "b"); code != exitExpect {
		t.Fatalf("exit %d, want %d", code, exitExpect)
	}
	status := readFile(t, filepath.Join(e.root, "work", "origin", "b", "status"))
	for _, want := range []string{"phase=asker step=1/3 exit=0", "phase=asker step=2/3 exit=10", "PRF_DONE branch=b phase=@ask exit=10"} {
		if !strings.Contains(status, want) {
			t.Errorf("status missing %q:\n%s", want, status)
		}
	}
}

func TestExpectOf(t *testing.T) {
	j := job{expect: expectCommits}
	role, err := parseRole("r", "r")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		p    phase
		want string
	}{
		{role, expectCommits},
		{phase{name: "r", tmpl: role.tmpl, expect: expectClean}, expectClean},
		{phase{name: "@check", tmpl: role.tmpl, gate: gateCheck}, ""},
		{phase{name: "prompt"}, ""},
	} {
		if got := j.expectOf(tt.p); got != tt.want {
			t.Errorf("expectOf(%s) = %q, want %q", tt.p.name, got, tt.want)
		}
	}
}

func TestExpectValidation(t *testing.T) {
	for _, tt := range []struct {
		s    step
		want string
	}{
		{step{Role: "test", Expect: "tidy"}, `step test: expect must be "commits", "read-only" or "clean", not "tidy"`},
		{step{Gate: gateCheck, Expect: expectCommits}, "step @check: expect is only for roles"},
	} {
		if err := checkGate(tt.s); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("checkGate(%+v) = %v, want %q", tt.s, err, tt.want)
		}
	}
	for _, v := range []string{expectCommits, expectReadOnly, expectClean} {
		if err := checkGate(step{Role: "test", Expect: v}); err != nil {
			t.Errorf("expect %s: %v", v, err)
		}
	}

	if b, _ := json.Marshal(step{Role: "test", Expect: expectClean}); string(b) != `{"role":"test","expect":"clean"}` {
		t.Errorf("a step with expect encodes as %s", b)
	}

	prev := home
	home = t.TempDir()
	t.Cleanup(func() { home = prev })
	writeFileT(t, filepath.Join(home, "app", pipelineFile), `{"expect": "sometimes", "pipelines": {}}`)
	if _, _, err := readPipelines("app"); err == nil || !strings.Contains(err.Error(), `expect must be "commits", "read-only" or "clean", not "sometimes"`) {
		t.Errorf("readPipelines = %v", err)
	}
}
