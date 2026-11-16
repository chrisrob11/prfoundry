package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chrisrob11/prfoundry/internal/state"
)

func readState(t *testing.T, jobDir string) state.Job {
	t.Helper()
	var s state.Job
	b, err := os.ReadFile(filepath.Join(jobDir, state.FileName))
	if err != nil || json.Unmarshal(b, &s) != nil {
		t.Fatalf("reading state: %v\n%s", err, b)
	}
	return s
}

func TestJobState(t *testing.T) {
	e := newEnv(t, "")
	writeRole(t, e.roles, "test", "SLOW test")
	e.h.rt.Heartbeat = 50 * time.Millisecond
	e.captureStderr()
	jobDir := filepath.Join(e.root, "work", "origin", "b")

	// Watch the job while it runs: during the slow test role, state.json
	// should say so, with Claude's latest action.
	done := make(chan int)
	go func() { done <- e.run(t, "run", "--repo", e.origin, "--branch", "b", "--prompt", "x") }()
	var during state.Job
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(filepath.Join(jobDir, state.FileName)); err == nil && json.Unmarshal(b, &during) == nil &&
			during.Stage == "test" && during.Activity != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if code := <-done; code != 0 {
		t.Fatalf("exit %d", code)
	}
	if during.State != state.Running || during.Step != 2 || during.Activity != "[bash] go test ./..." || during.PID != os.Getpid() {
		t.Errorf("state while test ran = %+v", during)
	}

	s := readState(t, jobDir)
	if s.State != state.Done || s.Stage != "review" || s.Step != 3 || s.Exit == nil || *s.Exit != 0 ||
		s.Pipeline != "default" || strings.Join(s.Roles, ",") != "implement,test,review" || s.Cost < 0.149 || s.Activity != "" {
		t.Errorf("final state = %+v", s)
	}
}

func TestStatusCommand(t *testing.T) {
	e := newEnv(t, "")
	e.captureStderr()
	if code := e.run(t, "run", "--repo", e.origin, "--branch", "ok", "--prompt", "x", "--role", "review"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	writeRole(t, e.roles, "test", "FAIL")
	if code := e.run(t, "run", "--repo", e.origin, "--branch", "bad", "--prompt", "x", "--role", "implement,test,review"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	// A job whose process is gone.
	dead := filepath.Join(e.root, "work", "other", "gone")
	b, _ := json.Marshal(state.Job{Repo: "other", Job: "gone", State: state.Running, Stage: "test", Step: 1, Roles: []string{"test"}, PID: 1 << 30, Started: time.Now(), Updated: time.Now().Add(-time.Hour)})
	writeFileT(t, filepath.Join(dead, state.FileName), string(b))

	code, out := e.runIO(t, "status", "--json")
	var states []state.Job
	if code != 0 || json.Unmarshal([]byte(out), &states) != nil {
		t.Fatalf("status --json = %d %s", code, out)
	}
	byJob := map[string]state.Job{}
	for _, s := range states {
		byJob[s.Repo+"/"+s.Job] = s
	}
	if s := byJob["origin/ok"]; s.State != state.Done || s.Step != 1 {
		t.Errorf("ok = %+v", s)
	}
	if s := byJob["origin/bad"]; s.State != state.Failed || s.Stage != "test" || s.Step != 2 || *s.Exit != 1 {
		t.Errorf("bad = %+v", s)
	}
	if s := byJob["other/gone"]; s.State != state.Died {
		t.Errorf("gone = %+v, want died", s)
	}
	if states[len(states)-1].Job != "gone" {
		t.Error("jobs should be listed most recently updated first")
	}

	_, text := e.runIO(t, "status", "--repo", "origin")
	for _, want := range []string{"REPO", "origin  bad", "failed", "2/3", "exit 1"} {
		if !strings.Contains(text, want) {
			t.Errorf("status text missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "gone") {
		t.Errorf("--repo origin should hide other repos:\n%s", text)
	}
	if _, out := e.runIO(t, "status", "--repo", "none", "--json"); strings.TrimSpace(out) != "[]" {
		t.Errorf("no jobs as JSON = %q, want []", out)
	}
}
