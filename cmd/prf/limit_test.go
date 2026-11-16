package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A step stopped by the usage limit pauses the job, then runs again and
// the job finishes: the limit never fails it.
func TestALimitPausesTheJobAndRerunsTheStep(t *testing.T) {
	e := newEnv(t, "")
	writeRole(t, e.roles, "test", "LIMIT")
	limits := filepath.Join(e.root, "limits")
	writeFileT(t, limits, "1\n")
	t.Setenv("FAKE_LIMITS", limits)
	var asked time.Time
	old := pauseFor
	pauseFor = func(reset time.Time) time.Duration { asked = reset; return 300 * time.Millisecond }
	t.Cleanup(func() { pauseFor = old })
	captureStderr(t)

	jobDir := filepath.Join(e.root, "work", "origin", "b")
	seen := make(chan jobState, 1)
	go func() {
		for range 100 {
			b, err := os.ReadFile(filepath.Join(jobDir, stateFile))
			if err == nil && strings.Contains(string(b), `"state": "paused"`) {
				seen <- readState(t, jobDir)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		close(seen)
	}()

	if code := e.run(t, "run", "--repo", e.origin, "--branch", "b", "--prompt", "x"); code != 0 {
		t.Fatalf("exit %d, want 0: the step runs again after the pause", code)
	}
	if n := len(e.calls(t)); n != 4 {
		t.Errorf("got %d claude calls, want 4 (implement, test twice, review)", n)
	}
	if asked.UTC().Hour() != 15 {
		t.Errorf("paused for a reset at %v, want 3pm UTC from the message", asked)
	}
	st, ok := <-seen
	if !ok {
		t.Fatal("state.json never said paused")
	}
	if st.Step != 2 || st.ResumeAt.IsZero() || !st.live() || !strings.Contains(detailOf(st), "usage limit") {
		t.Errorf("paused state = %+v, detail %q", st, detailOf(st))
	}
	if after := readState(t, jobDir); after.State != stateDone || !after.ResumeAt.IsZero() {
		t.Errorf("after: state %s, resume_at %v", after.State, after.ResumeAt)
	}
	b, _ := os.ReadFile(filepath.Join(jobDir, "status"))
	if !strings.Contains(string(b), "PRF_PAUSE branch=b phase=test step=2/3 until=") {
		t.Errorf("status has no pause line:\n%s", b)
	}
}

// A failure that isn't the usage limit still fails the job at once.
func TestAnotherErrorStillFailsAtOnce(t *testing.T) {
	e := newEnv(t, "")
	writeRole(t, e.roles, "test", "FAIL")
	old := pauseFor
	pauseFor = func(time.Time) time.Duration { t.Error("paused for a non-limit failure"); return 0 }
	t.Cleanup(func() { pauseFor = old })
	captureStderr(t)
	if code := e.run(t, "run", "--repo", e.origin, "--branch", "b", "--prompt", "x"); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
}
