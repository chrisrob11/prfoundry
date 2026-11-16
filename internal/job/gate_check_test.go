package job

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

	"github.com/chrisrob11/prfoundry/internal/config"
	"github.com/chrisrob11/prfoundry/internal/pipeline"
	"github.com/chrisrob11/prfoundry/internal/roles"
	"github.com/chrisrob11/prfoundry/internal/sandbox"
)

// gateJob is a job for calling a check gate directly: a checkout, a job
// folder, and the fake claude as its fix role.
func gateJob(t *testing.T) *Job {
	t.Helper()
	root := t.TempDir()
	claude := filepath.Join(root, "claude")
	writeFile(t, claude, fakeClaude)
	if err := os.Chmod(claude, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_LOG", filepath.Join(root, "claude.log"))
	j := &Job{
		name: "app", branch: "b", pipeline: "p", claude: claude, mode: "acceptEdits", stderr: io.Discard, grace: 300 * time.Millisecond, rt: Runtime{}.withDefaults(),
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

func TestRunCommandEdges(t *testing.T) {
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
	t.Run("interrupted while the command runs", func(t *testing.T) {
		j := gateJob(t)
		go func() {
			time.Sleep(300 * time.Millisecond)
			j.sigs <- os.Interrupt
		}()
		if code := j.commandGate(checkPhase(t, "echo started; sleep 30", "fix"), "2/3", "", roles.Data{}); code != 130 {
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
		if code := j.commandGate(checkPhase(t, "true", "COMMIT"), "1/1", "", roles.Data{}); code != ExitSetup {
			t.Fatalf("exit %d, want %d", code, ExitSetup)
		}
		if b, _ := os.ReadFile(os.Getenv("FAKE_LOG")); len(b) > 0 {
			t.Error("ran the fix role")
		}
	})

	t.Run("no TMPDIR for the command", func(t *testing.T) {
		j := gateJob(t)
		t.Setenv("TMPDIR", filepath.Join(j.jobDir, "missing"))
		if code := j.commandGate(checkPhase(t, "true", "fix"), "1/1", "", roles.Data{}); code != ExitSetup {
			t.Fatalf("exit %d, want %d", code, ExitSetup)
		}
	})

	t.Run("output that can't be saved doesn't stop the gate", func(t *testing.T) {
		j := gateJob(t)
		writeFile(t, filepath.Join(j.jobDir, "checks"), "a file, not a folder")
		j.tmp = filepath.Join(j.jobDir, "checks", "tmp") // unwritable too
		if code := j.commandGate(checkPhase(t, "exit 1", "fix\n{{.Review}}"), "1/1", "", roles.Data{}); code != ExitCheck {
			t.Fatalf("exit %d, want %d", code, ExitCheck)
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
		writeFile(t, settings, `{"permissions": {"deny": ["Bash(gh:*)"]}}`)
		if path, err := j.checkSandbox("/t"); path != "" || err != nil {
			t.Errorf("got %q, %v", path, err)
		}
	})
	t.Run("everything Claude's sandbox denies, the command's does", func(t *testing.T) {
		writeFile(t, settings, `{
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
		writeFile(t, settings, `{"sandbox": {"enabled": true, "network": {"allowedDomains": []}}}`)
		path, err := j.checkSandbox("/t")
		if err != nil {
			t.Fatal(err)
		}
		if out := readFile(t, path); strings.Contains(out, "allowLocalBinding") || strings.Contains(out, "UnixSockets") {
			t.Errorf("settings:\n%s", out)
		}
	})
	t.Run("a settings file that can't be written", func(t *testing.T) {
		writeFile(t, settings, `{"sandbox": {"enabled": true}}`)
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
	writeFile(t, filepath.Join(bin, "srt"), fakeSRT)
	if err := os.Chmod(filepath.Join(bin, "srt"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_SRT_LOG", filepath.Join(bin, "calls"))
}

func TestRunCommandInterrupted(t *testing.T) {
	j := &Job{dir: t.TempDir(), sigs: make(chan os.Signal, 1), grace: commandGrace}
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

func TestWriteOutput(t *testing.T) {
	var b strings.Builder
	writeOutput(&b, "\n")
	if !strings.Contains(b.String(), "printed nothing") {
		t.Errorf("empty output: %q", b.String())
	}
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

// The @check gate's srt sandbox may write where Claude's may.
func TestCheckSandboxJobWriteDirs(t *testing.T) {
	withFakeSRT(t)
	dir := t.TempDir()
	settings := filepath.Join(dir, sandbox.SettingsFile)
	writeFile(t, settings, `{"sandbox": {"enabled": true, "filesystem": {"allowWrite": ["/shared/go-build"]}}}`)
	j := Job{jobDir: dir, dir: filepath.Join(dir, "repo"), tmp: filepath.Join(dir, "tmp"), settings: settings,
		writeDirs: []string{filepath.Join(dir, "cache", "a"), filepath.Join(dir, "cache", "b")}}

	path, err := j.checkSandbox("/check-tmp")
	if err != nil {
		t.Fatal(err)
	}
	var s srtSettings
	if err := json.Unmarshal([]byte(readFile(t, path)), &s); err != nil {
		t.Fatal(err)
	}
	want := []string{j.dir, j.tmp, "/check-tmp", j.writeDirs[0], j.writeDirs[1], "/shared/go-build"}
	if !slices.Equal(s.Filesystem.AllowWrite, want) {
		t.Errorf("allowWrite = %v, want %v", s.Filesystem.AllowWrite, want)
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
	writeFile(t, j.settings, `{"sandbox": {"enabled": true, "filesystem": {"allowWrite": [], "denyRead": ["~/.ssh"]}, "network": {"allowedDomains": []}}}`)
	tmp, err := os.MkdirTemp("", "prf-check-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	sandbox, err := j.checkSandbox(tmp)
	if err != nil {
		t.Fatal(err)
	}
	home := config.ExpandHome("~")
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
