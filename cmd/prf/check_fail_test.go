package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/chrisrob11/prfoundry/internal/job"
	"github.com/chrisrob11/prfoundry/internal/pipeline"
)

func TestCheckGateResume(t *testing.T) {
	e := checkEnv(t, `{"gate": "check", "run": "sleep 5", "timeout": "200ms", "max_rounds": 1}`, `, "check": "exit 0"`)

	if code := e.run(t, "run", "--repo", e.origin, "--branch", "b", "--prompt", "x"); code != job.ExitCheck {
		t.Fatalf("exit %d, want %d", code, job.ExitCheck)
	}
	dir := filepath.Join(e.root, "work", "origin", "b", "checks", "step-2")
	writeFileT(t, filepath.Join(dir, "round-1.log"), "the first run's\n")

	// A resume keeps the step's timeout and numbers its rounds on.
	if code := e.run(t, "resume", "origin/b"); code != job.ExitCheck {
		t.Fatalf("resume: exit %d, want %d", code, job.ExitCheck)
	}
	status := readFile(t, filepath.Join(e.root, "work", "origin", "b", "status"))
	if !strings.Contains(status, "round=3 exit=124 stopped=timeout\n") || !strings.Contains(status, "round=4 exit=124 stopped=timeout\n") {
		t.Errorf("status:\n%s", status)
	}
	if readFile(t, filepath.Join(dir, "round-1.log")) != "the first run's\n" {
		t.Error("the resume overwrote the first run's output")
	}

	// A @check with no command of its own needs the repo's on resume too.
	e2 := checkEnv(t, `"@check"`, `, "check": "exit 1"`)
	writeRole(t, e2.roles, pipeline.Step{Gate: pipeline.GateCheck}.Fixer(), "fix {{.Review}}")
	if code := e2.run(t, "run", "--repo", e2.origin, "--branch", "c", "--prompt", "x"); code != job.ExitCheck {
		t.Fatalf("exit %d, want %d", code, job.ExitCheck)
	}
	writeFileT(t, filepath.Join(e2.home, "origin", pipeline.FileName), `{"pipelines": {}}`)
	code, msg := e2.runErr(t, "resume", "origin/c")
	if code != job.ExitUsage || !strings.Contains(msg, "@check has no command") {
		t.Errorf("exit %d: %s", code, msg)
	}
}

func TestCheckGateHangup(t *testing.T) {
	// A closed terminal stops the job, and the command with it, though the
	// command has a process group of its own.
	e := checkEnv(t, `{"gate": "check", "run": "echo $$ > ../started; sleep 30"}`, "")
	started := filepath.Join(e.root, "work", "origin", "b", "started")
	go func() {
		for range 200 {
			if _, err := os.Stat(started); err == nil && readFile(t, started) != "" {
				if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
					t.Error(err)
				}
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
	}()
	if code := e.run(t, "run", "--repo", e.origin, "--branch", "b", "--prompt", "x"); code != 130 {
		t.Fatalf("exit %d, want 130", code)
	}
	if alive(t, started) {
		t.Error("the command is still running")
	}
}

func TestCheckPipelineErrors(t *testing.T) {
	e := checkEnv(t, `"review"`, "")
	for _, tt := range []struct {
		steps []pipeline.Step
		want  string
	}{
		{[]pipeline.Step{{Gate: pipeline.GateCheck, Run: "make", FixRole: "nope"}}, `no role "nope"`},
		{[]pipeline.Step{{Role: "implement", MaxCost: -1}}, "max_cost can't be negative"},
		{[]pipeline.Step{{Role: "implement", Model: "bad model"}}, "invalid model"},
		{[]pipeline.Step{{Role: "implement", OpenCodeModel: "opus"}}, `opencode_model must be provider/model, not "opus"`},
		{[]pipeline.Step{{Gate: pipeline.GateCheck, Run: "make", Timeout: "0s"}}, `timeout "0s" isn't a duration`},
	} {
		if err := checkPipeline(e.home, "origin", tt.steps); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("checkPipeline(%+v) = %v, want %q", tt.steps, err, tt.want)
		}
	}
}

func TestJobTmpCantBeMade(t *testing.T) {
	e := checkEnv(t, `"review"`, "")
	writeFileT(t, filepath.Join(e.root, "work", "origin", "b", "tmp"), "a file, not a folder")
	if code := e.run(t, "run", "--repo", e.origin, "--branch", "b", "--prompt", "x"); code != job.ExitSetup {
		t.Fatalf("exit %d, want %d", code, job.ExitSetup)
	}
	if calls := e.calls(t); len(calls) != 0 {
		t.Errorf("ran %d steps", len(calls))
	}
}

// alive reports whether the process whose pid is in file is still running
// after a second: a killed process lingers until it's reaped.
func alive(t *testing.T, file string) bool {
	t.Helper()
	pid, err := strconv.Atoi(strings.TrimSpace(readFile(t, file)))
	if err != nil {
		t.Fatalf("pid file %s: %v", file, err)
	}
	for range 40 {
		if syscall.Kill(pid, 0) != nil {
			return false
		}
		time.Sleep(25 * time.Millisecond)
	}
	return true
}
