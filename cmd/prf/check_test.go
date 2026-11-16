package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/chrisrob11/prfoundry/internal/job"
	"github.com/chrisrob11/prfoundry/internal/pipeline"
	"github.com/chrisrob11/prfoundry/internal/sandbox"
)

// fakeSRT records its arguments, and the TMPDIR it got, in $FAKE_SRT_LOG,
// and runs the -c command, unsandboxed.
const fakeSRT = `#!/bin/sh
printf 'args=%s\ntmpdir=%s\n' "$*" "$TMPDIR" >> "$FAKE_SRT_LOG"
[ "$1" = -s ] && [ "$3" = -c ] || { echo "fake srt: bad args: $*" >&2; exit 64; }
exec sh -c "$4"
`

// withFakeSRT puts the fake srt first on PATH, logging to $FAKE_SRT_LOG.
func withFakeSRT(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	writeFileT(t, filepath.Join(bin, "srt"), fakeSRT)
	if err := os.Chmod(filepath.Join(bin, "srt"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_SRT_LOG", filepath.Join(bin, "calls"))
}

// checkEnv is a repo whose default pipeline is implement, then gate, then
// review, with a fix-check role that commits a change to fixed.txt. Jobs are
// sandboxed, so check commands go through the fake srt.
func checkEnv(t *testing.T, gate, extra string) env {
	t.Helper()
	e := newEnv(t, "")
	e.captureStderr()
	withFakeSRT(t)
	writeRole(t, e.roles, pipeline.Step{Gate: pipeline.GateCheck}.Fixer(), "COMMIT fix-check on {{.Branch}}\n{{.Review}}")
	writeFileT(t, filepath.Join(e.home, "origin", pipeline.FileName), `{"default": "a"`+extra+`, "pipelines": {"a": ["implement", `+gate+`, "review"]}}`)
	return e
}

// untilFixed fails, printing its environment, until fixed.txt exists.
const untilFixed = `test -f fixed.txt || { echo "TMPDIR=$TMPDIR PRF_TMP=$PRF_TMP"; echo "--- BROKE: TestX"; exit 1; }`

func TestCheckGate(t *testing.T) {
	e := checkEnv(t, `{"gate": "check", "run": "`+strings.ReplaceAll(untilFixed, `"`, `\"`)+`", "model": "opus"}`, "")

	if code := e.run(t, "run", "--repo", e.origin, "--branch", "b", "--prompt", "x"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	calls := e.calls(t)
	if len(calls) != 3 {
		t.Fatalf("got %d claude calls, want implement, fix-check, review", len(calls))
	}
	jobDir := filepath.Join(e.root, "work", "origin", "b")
	tmp := filepath.Join(jobDir, "tmp")
	fix := calls[1][1]
	for _, want := range []string{"COMMIT fix-check on b", "failed with exit code 1", "--- BROKE: TestX", "--model opus",
		"PRF_TMP=" + tmp, "TMPDIR=" + os.TempDir()} { // and a short TMPDIR of its own
		if !strings.Contains(fix, want) {
			t.Errorf("fix run's args missing %q:\n%s", want, fix)
		}
	}
	status := readFile(t, filepath.Join(jobDir, "status"))
	for _, want := range []string{
		"PRF_START branch=b phase=@check step=2/3 pipeline=a\n",
		"PRF_CHECK branch=b phase=@check step=2/3 round=1 exit=1\n",
		"PRF_PHASE branch=b phase=fix-check step=2/3 exit=0",
		"PRF_CHECK branch=b phase=@check step=2/3 round=2 exit=0\n",
		"PRF_PHASE branch=b phase=@check step=2/3 exit=0 rounds=2\n",
	} {
		if !strings.Contains(status, want) {
			t.Errorf("status missing %q:\n%s", want, status)
		}
	}
	dir := filepath.Join(jobDir, "checks", "step-2")
	if log := readFile(t, filepath.Join(dir, "round-1.log")); !strings.Contains(log, "--- BROKE: TestX") {
		t.Errorf("round-1.log = %q", log)
	}
	if md := readFile(t, filepath.Join(dir, "round-1.md")); !strings.Contains(md, "failed with exit code 1") {
		t.Errorf("round-1.md = %q", md)
	}
	if _, err := os.Stat(filepath.Join(dir, "round-2.md")); err == nil {
		t.Error("a passing round has no failure to save")
	}
	if log := readFile(t, filepath.Join(jobDir, "claude.log")); !strings.Contains(log, "--- BROKE: TestX") {
		t.Error("the command's output isn't in the job's log")
	}
}

func TestCheckGateRunsOutOfRounds(t *testing.T) {
	e := checkEnv(t, `{"gate": "check", "run": "echo nope; exit 3", "max_rounds": 2}`, "")

	if code := e.run(t, "run", "--repo", e.origin, "--branch", "b", "--prompt", "x"); code != job.ExitCheck {
		t.Fatalf("exit %d, want %d", code, job.ExitCheck)
	}
	if calls := e.calls(t); len(calls) != 3 {
		t.Fatalf("got %d claude calls, want implement and two fixes", len(calls))
	}
	jobDir := filepath.Join(e.root, "work", "origin", "b")
	status := readFile(t, filepath.Join(jobDir, "status"))
	for _, want := range []string{
		"round=3 exit=3\n",
		"PRF_PHASE branch=b phase=@check step=2/3 exit=9 rounds=3 stopped=max_rounds\n",
		"PRF_DONE branch=b phase=@check exit=9",
	} {
		if !strings.Contains(status, want) {
			t.Errorf("status missing %q:\n%s", want, status)
		}
	}

	// A resume runs the command again, with a fresh set of fixes.
	writeFileT(t, filepath.Join(e.home, "origin", pipeline.FileName), `{"default": "a", "pipelines": {"a": ["implement"]}}`)
	if code := e.run(t, "resume", "origin/b"); code != job.ExitCheck {
		t.Fatalf("resume: exit %d, want %d", code, job.ExitCheck)
	}
	if calls := e.calls(t); len(calls) != 5 {
		t.Fatalf("got %d claude calls after the resume, want two more fixes", len(calls))
	}
}

func TestCheckGateFixFails(t *testing.T) {
	e := checkEnv(t, `{"gate": "check", "run": "exit 1"}`, "")
	t.Setenv("FAKE_FAIL_ON", "fix-check")

	if code := e.run(t, "run", "--repo", e.origin, "--branch", "b", "--prompt", "x"); code != 1 {
		t.Fatalf("exit %d, want the fix role's 1", code)
	}
	status := readFile(t, filepath.Join(e.root, "work", "origin", "b", "status"))
	if !strings.Contains(status, "PRF_PHASE branch=b phase=@check step=2/3 exit=1 rounds=1\n") {
		t.Errorf("status:\n%s", status)
	}
}

func TestCheckGateRepoCommand(t *testing.T) {
	e := checkEnv(t, `"@check"`, `, "check": "test -f README.md"`)

	if code := e.run(t, "run", "--repo", e.origin, "--branch", "b", "--prompt", "x"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	// And on the command line.
	if code := e.run(t, "run", "--repo", e.origin, "--branch", "c", "--prompt", "x", "--role", "implement,@check"); code != 0 {
		t.Fatalf("--role: exit %d", code)
	}
	if calls := e.calls(t); len(calls) != 3 {
		t.Errorf("got %d claude calls, want no fixes", len(calls))
	}
}

func TestCheckGateTimeout(t *testing.T) {
	e := checkEnv(t, `{"gate": "check", "run": "sleep 30 & wait", "timeout": "200ms", "max_rounds": 1}`, "")

	start := time.Now()
	if code := e.run(t, "run", "--repo", e.origin, "--branch", "b", "--prompt", "x"); code != job.ExitCheck {
		t.Fatalf("exit %d, want %d", code, job.ExitCheck)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("took %s: the command's children weren't stopped", took)
	}
	if fix := e.calls(t)[1][1]; !strings.Contains(fix, "didn't finish within 200ms") {
		t.Errorf("fix run's args:\n%s", fix)
	}
	status := readFile(t, filepath.Join(e.root, "work", "origin", "b", "status"))
	if !strings.Contains(status, "round=1 exit=124 stopped=timeout\n") {
		t.Errorf("status:\n%s", status)
	}
}

func TestCheckValidation(t *testing.T) {
	for _, tt := range []struct {
		s    pipeline.Step
		want string
	}{
		{pipeline.Step{Gate: pipeline.GateReview, Run: "make"}, "run is only for the @check gate"},
		{pipeline.Step{Gate: pipeline.GateAsk, Timeout: "1m"}, "timeout is only for the @check gate"},
		{pipeline.Step{Gate: pipeline.GateCheck, Timeout: "soon"}, `timeout "soon" isn't a duration`},
		{pipeline.Step{Gate: pipeline.GateCheck, Timeout: "-1m"}, `timeout "-1m" isn't a duration`},
		{pipeline.Step{Role: "implement", Run: "make"}, "only for gates"},
		{pipeline.Step{Gate: "deploy"}, "unknown gate @deploy; the gates are @review, @ask and @check"},
	} {
		if err := pipeline.CheckGate(tt.s); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("CheckGate(%+v) = %v, want %q", tt.s, err, tt.want)
		}
	}
	for _, s := range []pipeline.Step{{Gate: pipeline.GateCheck, Run: "make", Timeout: "90s"}, {Gate: pipeline.GateCheck}} {
		if err := pipeline.CheckGate(s); err != nil {
			t.Errorf("CheckGate(%+v) = %v", s, err)
		}
	}
	steps := []pipeline.Step{{Role: "implement"}, {Gate: pipeline.GateCheck}}
	if err := pipeline.CheckCommands(steps, ""); err == nil || !strings.Contains(err.Error(), `step 2: @check has no command`) {
		t.Errorf("CheckCommands = %v", err)
	}
	if err := pipeline.CheckCommands(steps, "make test"); err != nil {
		t.Errorf("CheckCommands with the repo's = %v", err)
	}

	e := checkEnv(t, `"review"`, "")
	t.Run("pipeline set refuses a @check with nothing to run", func(t *testing.T) {
		code, out := e.runErr(t, "pipeline", "set", "--repo", "origin", "p", "--roles", "implement,@check")
		if code != job.ExitUsage || !strings.Contains(out, "@check has no command") {
			t.Errorf("exit %d: %s", code, out)
		}
	})
	t.Run("a run with @check and nothing to run stops before cloning", func(t *testing.T) {
		code, out := e.runErr(t, "run", "--repo", e.origin, "--branch", "x", "--prompt", "x", "--role", "implement,@check")
		if code != job.ExitUsage || !strings.Contains(out, "@check has no command") {
			t.Errorf("exit %d: %s", code, out)
		}
		if _, err := os.Stat(filepath.Join(e.root, "work", "origin", "x")); err == nil {
			t.Error("cloned anyway")
		}
	})
}

// srtSettings is the part of a check gate's srt settings file the tests read.
type srtSettings struct {
	Filesystem struct {
		AllowWrite, DenyWrite, DenyRead []string
	} `json:"filesystem"`
	Network struct {
		AllowedDomains []string `json:"allowedDomains"`
		DeniedDomains  []string `json:"deniedDomains"`
	} `json:"network"`
}

func TestCheckSandbox(t *testing.T) {
	e := checkEnv(t, `{"gate": "check", "run": "echo inside"}`, "")
	srtLog := os.Getenv("FAKE_SRT_LOG")
	jobDir := func(branch string) string { return filepath.Join(e.root, "work", "origin", branch) }

	t.Run("the command runs in srt with Claude's limits", func(t *testing.T) {
		if code := e.run(t, "run", "--repo", e.origin, "--branch", "a", "--prompt", "x"); code != 0 {
			t.Fatalf("exit %d", code)
		}
		var s srtSettings
		if err := json.Unmarshal([]byte(readFile(t, filepath.Join(jobDir("a"), "check-sandbox.json"))), &s); err != nil {
			t.Fatal(err)
		}
		repo, tmp := filepath.Join(jobDir("a"), "repo"), filepath.Join(jobDir("a"), "tmp")
		if len(s.Filesystem.AllowWrite) < 2 || s.Filesystem.AllowWrite[0] != repo || s.Filesystem.AllowWrite[1] != tmp ||
			!slices.Contains(s.Filesystem.DenyRead, "~/.ssh") || !slices.Contains(s.Network.AllowedDomains, "github.com") ||
			s.Filesystem.DenyWrite == nil || s.Network.DeniedDomains == nil {
			t.Errorf("srt settings = %+v", s)
		}
		calls := readFile(t, srtLog)
		if !strings.Contains(calls, "args=-s "+filepath.Join(jobDir("a"), "check-sandbox.json")+` -c TMPDIR="$PRF_CHECK_TMP" exec sh -c "$PRF_CHECK"`) {
			t.Errorf("srt calls:\n%s", calls)
		}
		if strings.Contains(calls, "tmpdir="+tmp+"\n") {
			t.Errorf("srt got the job's TMPDIR, too long a path for its socket:\n%s", calls)
		}
	})

	t.Run("without srt, the gate refuses rather than run unsandboxed", func(t *testing.T) {
		t.Setenv("PATH", "/usr/bin:/bin")
		removeFile(t, srtLog)
		errOut := e.captureStderr()
		if code := e.run(t, "run", "--repo", e.origin, "--branch", "b", "--prompt", "x"); code != job.ExitSetup {
			t.Fatalf("exit %d, want %d", code, job.ExitSetup)
		}
		if !strings.Contains(errOut.String(), "srt isn't installed: npm install -g @anthropic-ai/sandbox-runtime") {
			t.Errorf("stderr:\n%s", errOut)
		}
		if !strings.Contains(readFile(t, filepath.Join(jobDir("b"), "status")), "phase=@check step=2/3 exit=3 rounds=0\n") {
			t.Error("the gate didn't end with exit 3 before running anything")
		}
	})

	t.Run("a job run without the sandbox runs the command directly", func(t *testing.T) {
		removeFile(t, srtLog)
		if code := e.run(t, "run", "--repo", e.origin, "--branch", "c", "--prompt", "x", "--sandbox=false"); code != 0 {
			t.Fatalf("exit %d", code)
		}
		if readFile(t, srtLog) != "" {
			t.Error("used srt anyway")
		}
	})

	t.Run("so does one whose Claude settings turn the sandbox off", func(t *testing.T) {
		removeFile(t, srtLog)
		writeFileT(t, filepath.Join(e.home, "origin", sandbox.SettingsFile), `{"sandbox": {"enabled": false}}`)
		defer removeFile(t, filepath.Join(e.home, "origin", sandbox.SettingsFile))
		if code := e.run(t, "run", "--repo", e.origin, "--branch", "d", "--prompt", "x"); code != 0 {
			t.Fatalf("exit %d", code)
		}
		if readFile(t, srtLog) != "" {
			t.Error("used srt anyway")
		}
	})

	t.Run("unreadable Claude settings stop the gate", func(t *testing.T) {
		writeFileT(t, filepath.Join(e.home, "origin", sandbox.SettingsFile), `{oops`)
		defer removeFile(t, filepath.Join(e.home, "origin", sandbox.SettingsFile))
		if code := e.run(t, "run", "--repo", e.origin, "--branch", "e", "--prompt", "x"); code != job.ExitSetup {
			t.Fatalf("exit %d, want %d", code, job.ExitSetup)
		}
	})
}

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
