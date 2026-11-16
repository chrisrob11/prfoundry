package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/chrisrob11/prfoundry/internal/claude"
	"github.com/chrisrob11/prfoundry/internal/roles"
	"github.com/chrisrob11/prfoundry/internal/state"
)

// A step that runs again in the same job is told where its last run ended,
// and where its report is. A step running for the first time isn't.
func TestPriorRun(t *testing.T) {
	e := newEnv(t, "")
	captureStderr(t)
	writeRole(t, e.roles, "test", "COMMIT FAIL")
	if code := e.run(t, "run", "--repo", e.origin, "--branch", "b", "--prompt", "x"); code != 1 {
		t.Fatalf("run: exit %d, want 1", code)
	}
	for _, c := range e.calls(t) {
		if strings.Contains(c[1], "This step ran before") {
			t.Errorf("a first run got a re-run note: %s", c[1])
		}
	}
	jobDir := filepath.Join(e.root, "work", "origin", "b")
	st := readState(t, jobDir)
	clone := filepath.Join(jobDir, "repo")
	head := mustGit(t, clone, "rev-parse", "HEAD")
	if len(st.StepHeads) != 2 || st.StepHeads[2] != head || st.StepHeads[1] == "" {
		t.Errorf("step_heads = %v, want step 2 at %s", st.StepHeads, head)
	}
	report := filepath.Join(jobDir, "tmp", "reports", "step-2.md")
	if got := readFile(t, report); got != "it broke\n" {
		t.Errorf("step 2's report = %q", got)
	}

	writeRole(t, e.roles, "test", "test on {{.Branch}} after {{.PriorRun}}")
	removeFile(t, e.log)
	if code := e.run(t, "resume", "origin/b"); code != 0 {
		t.Fatalf("resume: exit %d", code)
	}
	calls := e.calls(t)
	if len(calls) != 2 {
		t.Fatalf("resume ran %d steps, want 2", len(calls))
	}
	note := "## This step ran before\n\nThis step ran before, ending at " + head + ": read `git diff " + head + "..HEAD` and its earlier report (" + report + ") first, and read unchanged code only where you need it."
	if !strings.Contains(calls[0][1], "after "+head) || !strings.Contains(calls[0][1], note) {
		t.Errorf("the re-run's prompt should have the sha and the note:\n%s", calls[0][1])
	}
	if strings.Contains(calls[1][1], "This step ran before") {
		t.Errorf("step 3 never ran before, but got the note:\n%s", calls[1][1])
	}
	if st := readState(t, jobDir); len(st.StepHeads) != 3 {
		t.Errorf("step_heads after resume = %v", st.StepHeads)
	}
}

func TestRecordRun(t *testing.T) {
	clone := filepath.Join(t.TempDir(), "repo")
	mustGit(t, "", "clone", "-q", newOrigin(t), clone)
	j := &job{dir: clone, jobDir: t.TempDir()}
	j.tmp = filepath.Join(j.jobDir, "tmp")
	head := mustGit(t, clone, "rev-parse", "HEAD")

	j.lastResult = claude.Result{Text: "did it"}
	j.recordRun(1)
	if got := j.priorRun(1, roles.Data{}); got.PriorRun != head || got.PriorReport != j.reportPath(1) {
		t.Errorf("after a run with a report: %+v", got)
	}
	if got := j.priorRun(2, roles.Data{}); got.PriorRun != "" || got.PriorReport != "" {
		t.Errorf("a step that never ran: %+v", got)
	}

	j.lastResult = claude.Result{}
	j.recordRun(1)
	if got := j.priorRun(1, roles.Data{}); got.PriorRun != head || got.PriorReport != "" {
		t.Errorf("a run with no report should drop the older one: %+v", got)
	}

	logs := captureStderr(t)
	j.tmp = filepath.Join(j.jobDir, state.FileName) // a file, so reports/ can't be made
	j.lastResult = claude.Result{Text: "did it"}
	j.recordRun(1)
	if !strings.Contains(logs.String(), "saving step 1's report: ") {
		t.Errorf("log = %q", logs.String())
	}
}
