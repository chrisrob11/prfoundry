package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/chrisrob11/prfoundry/internal/pipeline"
)

// gateJob is a job for calling a check gate directly: a checkout, a job
// folder, and the fake claude as its fix role.
func gateJob(t *testing.T) *job {
	t.Helper()
	root := t.TempDir()
	claude := filepath.Join(root, "claude")
	writeFileT(t, claude, fakeClaude)
	if err := os.Chmod(claude, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_LOG", filepath.Join(root, "claude.log"))
	captureStderr(t)
	j := &job{
		name: "app", branch: "b", pipeline: "p", claude: claude, mode: "acceptEdits",
		jobDir: filepath.Join(root, "job"), dir: filepath.Join(root, "repo"), tmp: filepath.Join(root, "job", "tmp"),
		status: filepath.Join(root, "job", "status"), env: os.Environ(), sigs: make(chan os.Signal, 1),
		log: io.Discard, raw: io.Discard, feed: io.Discard, summaries: io.Discard,
	}
	for _, d := range []string{j.dir, j.tmp} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return j
}

// checkPhase is a check gate running command, with fix role prompt and one round.
func checkPhase(t *testing.T, command, prompt string) phase {
	t.Helper()
	p, err := parseRole("fix-check", prompt)
	if err != nil {
		t.Fatal(err)
	}
	p.name, p.gate, p.fixRole, p.run, p.maxRounds = "@check", pipeline.GateCheck, "fix-check", command, 1
	return p
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

func withGrace(t *testing.T, d time.Duration) {
	prev := commandGrace
	commandGrace = d
	t.Cleanup(func() { commandGrace = prev })
}

func TestRunCommandEdges(t *testing.T) {
	withGrace(t, 300*time.Millisecond)
	j := gateJob(t)
	pidFile := filepath.Join(j.tmp, "pid")

	t.Run("a passing command that leaves a child running passes, and the child is killed", func(t *testing.T) {
		out, code, err := j.runCommand("sleep 30 & echo $! > "+pidFile+"; echo done; exit 0", 0, checkRun{})
		if code != 0 || err != nil || out != "done\n" {
			t.Errorf("got %q, %d, %v", out, code, err)
		}
		if alive(t, pidFile) {
			t.Error("the child is still running")
		}
	})

	t.Run("a command that ends before its timeout isn't a timeout", func(t *testing.T) {
		// It exits at once, but its child holds the output past the deadline.
		_, code, err := j.runCommand("sleep 2 & exit 0", 100*time.Millisecond, checkRun{})
		if code != 0 || err != nil {
			t.Errorf("got %d, %v; want a pass", code, err)
		}
	})

	t.Run("a command killed by a signal", func(t *testing.T) {
		if _, code, err := j.runCommand("kill -9 $$", 0, checkRun{}); code != 137 || err != nil {
			t.Errorf("got %d, %v; want 137", code, err)
		}
	})

	t.Run("a stopped command gets SIGTERM, and time to clean up", func(t *testing.T) {
		go func() {
			time.Sleep(200 * time.Millisecond)
			j.sigs <- syscall.SIGHUP
		}()
		out, code, err := j.runCommand("trap 'echo cleaned up; exit 1' TERM; echo started; sleep 30 & wait", 0, checkRun{})
		if !errors.Is(err, errInterrupted) || code != 130 || !strings.Contains(out, "cleaned up") {
			t.Errorf("got %q, %d, %v", out, code, err)
		}
	})

	t.Run("one that ignores SIGTERM is killed after the grace period", func(t *testing.T) {
		start := time.Now()
		out, code, err := j.runCommand("trap '' TERM; echo started; sleep 30", 200*time.Millisecond, checkRun{})
		if code != 124 || err == nil || out != "started\n" {
			t.Errorf("got %q, %d, %v", out, code, err)
		}
		if took := time.Since(start); took > 5*time.Second {
			t.Errorf("took %s", took)
		}
	})

	t.Run("the command gets its own TMPDIR, and the job's PRF_TMP", func(t *testing.T) {
		out, _, _ := j.runCommand(`echo "$TMPDIR $PRF_TMP"`, 0, checkRun{tmp: "/short/tmp"})
		if !strings.HasPrefix(out, "/short/tmp ") {
			t.Errorf("TMPDIR and PRF_TMP = %q", out)
		}
	})
}

func TestCommandGateFailures(t *testing.T) {
	withGrace(t, 300*time.Millisecond)

	t.Run("interrupted while the command runs", func(t *testing.T) {
		j := gateJob(t)
		go func() {
			time.Sleep(300 * time.Millisecond)
			j.sigs <- os.Interrupt
		}()
		if code := j.commandGate(checkPhase(t, "echo started; sleep 30", "fix"), "2/3", "", promptData{}); code != 130 {
			t.Fatalf("exit %d, want 130", code)
		}
		if log := readFile(t, filepath.Join(j.jobDir, "checks", "step-2", "round-1.log")); log != "started\n" {
			t.Errorf("round-1.log = %q", log)
		}
		if status := readFile(t, j.status); !strings.Contains(status, "phase=@check step=2/3 exit=130 rounds=1\n") {
			t.Errorf("status:\n%s", status)
		}
	})

	t.Run("a command that can't start", func(t *testing.T) {
		j := gateJob(t)
		if err := os.RemoveAll(j.dir); err != nil {
			t.Fatal(err)
		}
		if code := j.commandGate(checkPhase(t, "true", "COMMIT"), "1/1", "", promptData{}); code != exitSetup {
			t.Fatalf("exit %d, want %d", code, exitSetup)
		}
		if b, _ := os.ReadFile(os.Getenv("FAKE_LOG")); len(b) > 0 {
			t.Error("ran the fix role")
		}
	})

	t.Run("no TMPDIR for the command", func(t *testing.T) {
		j := gateJob(t)
		t.Setenv("TMPDIR", filepath.Join(j.jobDir, "missing"))
		if code := j.commandGate(checkPhase(t, "true", "fix"), "1/1", "", promptData{}); code != exitSetup {
			t.Fatalf("exit %d, want %d", code, exitSetup)
		}
	})

	t.Run("output that can't be saved doesn't stop the gate", func(t *testing.T) {
		j := gateJob(t)
		writeFileT(t, filepath.Join(j.jobDir, "checks"), "a file, not a folder")
		j.tmp = filepath.Join(j.jobDir, "checks", "tmp") // unwritable too
		if code := j.commandGate(checkPhase(t, "exit 1", "fix\n{{.Review}}"), "1/1", "", promptData{}); code != exitCheck {
			t.Fatalf("exit %d, want %d", code, exitCheck)
		}
		fix := readFile(t, os.Getenv("FAKE_LOG"))
		if !strings.Contains(fix, "failed with exit code 1") || strings.Contains(fix, "The full output is in") {
			t.Errorf("fix role's prompt:\n%s", fix)
		}
	})
}

func TestCheckSandboxSettings(t *testing.T) {
	withFakeSRT(t)
	j := gateJob(t)
	settings := filepath.Join(j.jobDir, "claude-settings.json")
	j.settings = settings

	t.Run("missing", func(t *testing.T) {
		if _, err := j.checkSandbox("/t"); err == nil || !strings.Contains(err.Error(), "reading the Claude settings") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("no sandbox section: unsandboxed", func(t *testing.T) {
		writeFileT(t, settings, `{"permissions": {"deny": ["Bash(gh:*)"]}}`)
		if path, err := j.checkSandbox("/t"); path != "" || err != nil {
			t.Errorf("got %q, %v", path, err)
		}
	})
	t.Run("everything Claude's sandbox denies, the command's does", func(t *testing.T) {
		writeFileT(t, settings, `{
			"sandbox": {"enabled": true, "enableWeakerNetworkIsolation": true,
				"filesystem": {"allowWrite": ["/cache"], "denyWrite": ["/etc/app"], "denyRead": ["~/.ssh"]},
				"network": {"allowedDomains": ["github.com"], "allowLocalBinding": true, "allowUnixSockets": ["/var/run/x.sock"], "allowAllUnixSockets": true}},
			"permissions": {"deny": ["Read(~/.kube/**)", "Read(~/.netrc)", "Edit(~/notes/*)", "Bash(gh:*)", "Read()"]}}`)
		path, err := j.checkSandbox("/t")
		if err != nil {
			t.Fatal(err)
		}
		var s srtSettings
		if err := json.Unmarshal([]byte(readFile(t, path)), &s); err != nil {
			t.Fatal(err)
		}
		if want := []string{j.dir, j.tmp, "/t", "/cache"}; !slices.Equal(s.Filesystem.AllowWrite, want) {
			t.Errorf("allowWrite = %v, want %v", s.Filesystem.AllowWrite, want)
		}
		if want := []string{filepath.Join(j.dir, ".claude"), filepath.Join(j.dir, ".mcp.json"), "/etc/app", "~/notes"}; !slices.Equal(s.Filesystem.DenyWrite, want) {
			t.Errorf("denyWrite = %v, want %v", s.Filesystem.DenyWrite, want)
		}
		if want := []string{"~/.ssh", "~/.kube", "~/.netrc"}; !slices.Equal(s.Filesystem.DenyRead, want) {
			t.Errorf("denyRead = %v, want %v", s.Filesystem.DenyRead, want)
		}
		if !s.EnableWeakerNetworkIsolation || !slices.Equal(s.Network.AllowedDomains, []string{"github.com"}) || s.Network.DeniedDomains == nil ||
			!s.Network.AllowLocalBinding || !slices.Equal(s.Network.AllowUnixSockets, []string{"/var/run/x.sock"}) || !s.Network.AllowAllUnixSockets {
			t.Errorf("settings = %+v", s)
		}
	})
	t.Run("without local binding in Claude's settings, the command gets none", func(t *testing.T) {
		writeFileT(t, settings, `{"sandbox": {"enabled": true, "network": {"allowedDomains": []}}}`)
		path, err := j.checkSandbox("/t")
		if err != nil {
			t.Fatal(err)
		}
		if out := readFile(t, path); strings.Contains(out, "allowLocalBinding") || strings.Contains(out, "UnixSockets") {
			t.Errorf("settings:\n%s", out)
		}
	})
	t.Run("a settings file that can't be written", func(t *testing.T) {
		writeFileT(t, settings, `{"sandbox": {"enabled": true}}`)
		path := filepath.Join(j.jobDir, "check-sandbox.json")
		removeFile(t, path)
		if err := os.MkdirAll(filepath.Join(path, "x"), 0o755); err != nil { // a folder in the way
			t.Fatal(err)
		}
		defer func() {
			if err := os.RemoveAll(path); err != nil {
				t.Error(err)
			}
		}()
		if _, err := j.checkSandbox("/t"); err == nil {
			t.Error("no error")
		}
	})
}

func TestPermissionPath(t *testing.T) {
	for rule, want := range map[string]string{
		"Read(~/.ssh/**)": "~/.ssh", "Read(~/.netrc)": "~/.netrc", "Read(/a/*)": "/a",
		"Read()": "", "Read(/**)": "", "Edit(/x)": "", "Read(~/.ssh": "", "Bash(gh:*)": "",
	} {
		got, ok := permissionPath(rule, "Read")
		if got != want || ok != (want != "") {
			t.Errorf("permissionPath(%q) = %q, %t; want %q", rule, got, ok, want)
		}
	}
}

func TestCheckGateResume(t *testing.T) {
	e := checkEnv(t, `{"gate": "check", "run": "sleep 5", "timeout": "200ms", "max_rounds": 1}`, `, "check": "exit 0"`)

	if code := e.run(t, "run", "--repo", e.origin, "--branch", "b", "--prompt", "x"); code != exitCheck {
		t.Fatalf("exit %d, want %d", code, exitCheck)
	}
	dir := filepath.Join(e.root, "work", "origin", "b", "checks", "step-2")
	writeFileT(t, filepath.Join(dir, "round-1.log"), "the first run's\n")

	// A resume keeps the step's timeout and numbers its rounds on.
	if code := e.run(t, "resume", "origin/b"); code != exitCheck {
		t.Fatalf("resume: exit %d, want %d", code, exitCheck)
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
	if code := e2.run(t, "run", "--repo", e2.origin, "--branch", "c", "--prompt", "x"); code != exitCheck {
		t.Fatalf("exit %d, want %d", code, exitCheck)
	}
	writeFileT(t, filepath.Join(e2.home, "origin", pipeline.FileName), `{"pipelines": {}}`)
	code, msg := runPrfErr(t, "--home", e2.home, "resume", "origin/c")
	if code != exitUsage || !strings.Contains(msg, "@check has no command") {
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
	prev := home
	home = e.home
	t.Cleanup(func() { home = prev })
	for _, tt := range []struct {
		steps []pipeline.Step
		want  string
	}{
		{[]pipeline.Step{{Gate: pipeline.GateCheck, Run: "make", FixRole: "nope"}}, `no role "nope"`},
		{[]pipeline.Step{{Role: "implement", MaxCost: -1}}, "max_cost can't be negative"},
		{[]pipeline.Step{{Role: "implement", Model: "bad model"}}, "invalid model"},
		{[]pipeline.Step{{Gate: pipeline.GateCheck, Run: "make", Timeout: "0s"}}, `timeout "0s" isn't a duration`},
	} {
		if err := checkPipeline("origin", tt.steps); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("checkPipeline(%+v) = %v, want %q", tt.steps, err, tt.want)
		}
	}
}

func TestJobTmpCantBeMade(t *testing.T) {
	e := checkEnv(t, `"review"`, "")
	writeFileT(t, filepath.Join(e.root, "work", "origin", "b", "tmp"), "a file, not a folder")
	if code := e.run(t, "run", "--repo", e.origin, "--branch", "b", "--prompt", "x"); code != exitSetup {
		t.Fatalf("exit %d, want %d", code, exitSetup)
	}
	if calls := e.calls(t); len(calls) != 0 {
		t.Errorf("ran %d steps", len(calls))
	}
}

// TestRealSRT runs a command in the real srt, when it's installed: what the
// sandbox lets it do, and what it doesn't.
func TestRealSRT(t *testing.T) {
	if _, err := exec.LookPath("srt"); err != nil {
		t.Skip("srt isn't installed")
	}
	j := gateJob(t)
	j.env = append(j.env, "PRF_TMP="+j.tmp, "JOB_CACHE="+filepath.Join(j.jobDir, "cache", "go-build"))
	j.writeDirs = []string{filepath.Join(j.jobDir, "cache", "go-build")}
	if err := os.MkdirAll(j.writeDirs[0], 0o755); err != nil {
		t.Fatal(err)
	}
	j.settings = filepath.Join(j.jobDir, "claude-settings.json")
	writeFileT(t, j.settings, `{"sandbox": {"enabled": true, "filesystem": {"allowWrite": [], "denyRead": ["~/.ssh"]}, "network": {"allowedDomains": []}}}`)
	tmp, err := os.MkdirTemp("", "prf-check-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	sandbox, err := j.checkSandbox(tmp)
	if err != nil {
		t.Fatal(err)
	}
	home := homeDir()
	out, code, err := j.runCommand(`echo "tmp=$TMPDIR task=$PRF_TMP"
touch ok && echo checkout-writable
touch "$TMPDIR/x" && echo tmp-writable
touch "$PRF_TMP/x" && echo job-tmp-writable
touch "$JOB_CACHE/x" && echo job-cache-writable
touch "$JOB_CACHE/../../state.json" 2>/dev/null && echo JOB-FOLDER-WRITABLE
touch "$JOB_CACHE/../sibling" 2>/dev/null && echo CACHE-SIBLING-WRITABLE
mkdir -p .claude && echo '{}' > .claude/settings.json 2>/dev/null && echo CLAUDE-SETTINGS-WRITABLE
touch "`+home+`/prf-srt-probe" 2>/dev/null && echo HOME-WRITABLE
ls ~/.ssh >/dev/null 2>&1 && echo SSH-READABLE
echo "shell=$0"
exit 3`, 0, checkRun{sandbox: sandbox, tmp: tmp})
	removeFile(t, filepath.Join(home, "prf-srt-probe"))
	if err != nil || code != 3 {
		t.Fatalf("got %d, %v:\n%s", code, err, out)
	}
	for _, want := range []string{"tmp=" + tmp + " task=" + j.tmp, "shell=sh\n", "checkout-writable", "tmp-writable", "job-tmp-writable", "job-cache-writable"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	for _, not := range []string{"CLAUDE-SETTINGS-WRITABLE", "HOME-WRITABLE", "SSH-READABLE", "JOB-FOLDER-WRITABLE", "CACHE-SIBLING-WRITABLE"} {
		if strings.Contains(out, not) {
			t.Errorf("the sandbox allowed %s:\n%s", not, out)
		}
	}
}
