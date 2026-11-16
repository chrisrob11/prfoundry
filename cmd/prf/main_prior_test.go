package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// A step that runs again in the same job is told where its last run ended,
// and where its report is. A step running for the first time isn't.
func TestPriorRun(t *testing.T) {
	e := newEnv(t, "")
	e.captureStderr()
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
	head, err := gitOut(clone, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
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
