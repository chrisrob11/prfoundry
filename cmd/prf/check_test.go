package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
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
	captureStderr(t)
	withFakeSRT(t)
	writeRole(t, e.roles, gateFixRoles[gateCheck], "COMMIT fix-check on {{.Branch}}\n{{.Review}}")
	writeFileT(t, filepath.Join(e.home, "origin", pipelineFile), `{"default": "a"`+extra+`, "pipelines": {"a": ["implement", `+gate+`, "review"]}}`)
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

	if code := e.run(t, "run", "--repo", e.origin, "--branch", "b", "--prompt", "x"); code != exitCheck {
		t.Fatalf("exit %d, want %d", code, exitCheck)
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
	writeFileT(t, filepath.Join(e.home, "origin", pipelineFile), `{"default": "a", "pipelines": {"a": ["implement"]}}`)
	if code := e.run(t, "resume", "origin/b"); code != exitCheck {
		t.Fatalf("resume: exit %d, want %d", code, exitCheck)
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
	if code := e.run(t, "run", "--repo", e.origin, "--branch", "b", "--prompt", "x"); code != exitCheck {
		t.Fatalf("exit %d, want %d", code, exitCheck)
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

func TestRunCommandInterrupted(t *testing.T) {
	j := &job{dir: t.TempDir(), sigs: make(chan os.Signal, 1)}
	go func() {
		time.Sleep(100 * time.Millisecond)
		j.sigs <- os.Interrupt
	}()
	start := time.Now()
	out, code, err := j.runCommand("echo started; sleep 30", 0, checkRun{})
	if !errors.Is(err, errInterrupted) || code != 130 || out != "started\n" {
		t.Errorf("got %q, %d, %v", out, code, err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("took %s", took)
	}
	if _, _, err := j.runCommand("exit 0", 0, checkRun{}); err != nil {
		t.Errorf("a clean run: %v", err)
	}
	j.dir = filepath.Join(j.dir, "gone")
	if _, _, err := j.runCommand("exit 0", 0, checkRun{}); err == nil {
		t.Error("a command that can't start should fail")
	}
}

func TestCheckValidation(t *testing.T) {
	for _, tt := range []struct {
		s    step
		want string
	}{
		{step{Gate: gateReview, Run: "make"}, "run is only for the @check gate"},
		{step{Gate: gateAsk, Timeout: "1m"}, "timeout is only for the @check gate"},
		{step{Gate: gateCheck, Timeout: "soon"}, `timeout "soon" isn't a duration`},
		{step{Gate: gateCheck, Timeout: "-1m"}, `timeout "-1m" isn't a duration`},
		{step{Role: "implement", Run: "make"}, "only for gates"},
		{step{Gate: "deploy"}, "unknown gate @deploy; the gates are @review, @ask and @check"},
	} {
		if err := checkGate(tt.s); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("checkGate(%+v) = %v, want %q", tt.s, err, tt.want)
		}
	}
	for _, s := range []step{{Gate: gateCheck, Run: "make", Timeout: "90s"}, {Gate: gateCheck}} {
		if err := checkGate(s); err != nil {
			t.Errorf("checkGate(%+v) = %v", s, err)
		}
	}
	steps := []step{{Role: "implement"}, {Gate: gateCheck}}
	if err := checkCommands(steps, ""); err == nil || !strings.Contains(err.Error(), `step 2: @check has no command`) {
		t.Errorf("checkCommands = %v", err)
	}
	if err := checkCommands(steps, "make test"); err != nil {
		t.Errorf("checkCommands with the repo's = %v", err)
	}

	e := checkEnv(t, `"review"`, "")
	t.Run("pipeline set refuses a @check with nothing to run", func(t *testing.T) {
		code, out := runPrfErr(t, "--home", e.home, "pipeline", "set", "--repo", "origin", "p", "--roles", "implement,@check")
		if code != exitUsage || !strings.Contains(out, "@check has no command") {
			t.Errorf("exit %d: %s", code, out)
		}
	})
	t.Run("a run with @check and nothing to run stops before cloning", func(t *testing.T) {
		code, out := runPrfErr(t, "--home", e.home, "run", "--repo", e.origin, "--branch", "x", "--prompt", "x", "--role", "implement,@check")
		if code != exitUsage || !strings.Contains(out, "@check has no command") {
			t.Errorf("exit %d: %s", code, out)
		}
		if _, err := os.Stat(filepath.Join(e.root, "work", "origin", "x")); err == nil {
			t.Error("cloned anyway")
		}
	})
}

func TestWriteOutput(t *testing.T) {
	var b strings.Builder
	writeOutput(&b, "\n")
	if !strings.Contains(b.String(), "printed nothing") {
		t.Errorf("empty output: %q", b.String())
	}
}

// runPrfErr runs the app in-process, returning its exit code and the
// message it exits with.
func runPrfErr(t *testing.T, args ...string) (int, string) {
	t.Helper()
	app := newApp()
	app.Reader, app.Writer = strings.NewReader(""), io.Discard
	err := app.Run(context.Background(), append([]string{"prf"}, args...))
	if err == nil {
		return 0, ""
	}
	return exitCode(t, err), err.Error()
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
		errOut := captureStderr(t)
		if code := e.run(t, "run", "--repo", e.origin, "--branch", "b", "--prompt", "x"); code != exitSetup {
			t.Fatalf("exit %d, want %d", code, exitSetup)
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
		writeFileT(t, filepath.Join(e.home, "origin", settingsFile), `{"sandbox": {"enabled": false}}`)
		defer removeFile(t, filepath.Join(e.home, "origin", settingsFile))
		if code := e.run(t, "run", "--repo", e.origin, "--branch", "d", "--prompt", "x"); code != 0 {
			t.Fatalf("exit %d", code)
		}
		if readFile(t, srtLog) != "" {
			t.Error("used srt anyway")
		}
	})

	t.Run("unreadable Claude settings stop the gate", func(t *testing.T) {
		writeFileT(t, filepath.Join(e.home, "origin", settingsFile), `{oops`)
		defer removeFile(t, filepath.Join(e.home, "origin", settingsFile))
		if code := e.run(t, "run", "--repo", e.origin, "--branch", "e", "--prompt", "x"); code != exitSetup {
			t.Fatalf("exit %d, want %d", code, exitSetup)
		}
	})
}

func TestRenderFailure(t *testing.T) {
	goOut := "ok  \tapp/a\t0.1s\n--- FAIL: TestAdd (0.00s)\n    add_test.go:7: got -1, want 5\nFAIL\nFAIL\tapp/b\t0.2s\n"
	got := renderFailure("go test ./...", 1, false, 0, goOut, "/job/tmp/check-2-round-1.log")
	for _, want := range []string{"`go test ./...` failed with exit code 1.", "The failures in its output:",
		"--- FAIL: TestAdd", "add_test.go:7: got -1, want 5", "FAIL\tapp/b", "The full output is in /job/tmp/check-2-round-1.log."} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "ok  \tapp/a") {
		t.Errorf("kept a passing package:\n%s", got)
	}

	got = renderFailure("make", 2, false, 0, "something broke\n", "")
	if !strings.Contains(got, "Its output:") || !strings.Contains(got, "something broke") || strings.Contains(got, "full output") {
		t.Errorf("unknown output:\n%s", got)
	}
	got = renderFailure("false", 1, false, 0, "", "")
	if !strings.Contains(got, "It printed nothing.") {
		t.Errorf("no output:\n%s", got)
	}
	got = renderFailure("go test ./...", 137, false, 0, goOut, "")
	if !strings.Contains(got, "was killed by signal 9 (exit code 137)") {
		t.Errorf("killed:\n%s", got)
	}
	// What the excerpt doesn't recognize, at the end, is shown too.
	got = renderFailure("make check", 2, false, 0, goOut+"make: *** [check] Error 2\n", "")
	if !strings.Contains(got, "The end of its output:") || !strings.Contains(got, "make: *** [check] Error 2") {
		t.Errorf("unrecognized end:\n%s", got)
	}
	if strings.Contains(renderFailure("go test ./...", 1, false, 0, goOut, ""), "The end of its output:") {
		t.Error("repeated the end when the excerpt already had it")
	}
	got = renderFailure("go test ./...", 124, true, time.Minute, goOut+"=== RUN TestSlow\n", "")
	if !strings.Contains(got, "didn't finish within 1m0s") || !strings.Contains(got, "=== RUN TestSlow") {
		t.Errorf("a timeout shows the end of the output:\n%s", got)
	}
}
