package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// A check gate (@check in a pipeline) runs a command in the checkout, such
// as the tests, and passes when it exits 0. When it fails, the gate's fix
// role gets what failed in {{.Review}} (see excerpt) and fixes the branch;
// then the gate runs the command again, up to max_rounds fixes. The command
// runs the code the steps wrote, so when the job is sandboxed it runs in the
// same sandbox as Claude, through srt (Anthropic's sandbox runtime), with the
// limits in the job's Claude settings. Each run's output is saved in
// <job>/checks/step-N/round-M.log, a copy in the job's tmp folder for the fix
// role to read, and the failure as the fix role saw it in round-M.md.

// How much of a failure's output a fix role reads: its last lines, up to
// this many, and at most this many bytes.
const (
	failureLines = 200
	failureBytes = 30_000
)

// errInterrupted is what a gate's wait returns when you press Ctrl-C.
var errInterrupted = errors.New("interrupted")

// commandGrace is how long a command prf stops, or the processes a
// finished command left running, get to exit before they're killed.
var commandGrace = 5 * time.Second

// checkRun is how a check gate runs its command: in srt with the settings
// file sandbox, unless that's "", and with tmp as its TMPDIR.
type checkRun struct {
	sandbox, tmp string
}

// commandGate runs the check gate p at step and returns its exit code: 0
// once its command passes, the fix role's code if it fails, exitCheck when
// the command still fails after max_rounds fixes, exitSetup if the command
// can't start or be sandboxed, or 130 if interrupted. A resumed gate numbers
// its rounds after the ones already saved.
func (j *job) commandGate(p phase, step, model string, data promptData) int {
	n, _, _ := strings.Cut(step, "/")
	dir := filepath.Join(j.jobDir, "checks", "step-"+n)
	command := firstNonEmpty(p.run, j.checkCmd)
	saved, _ := filepath.Glob(filepath.Join(dir, "round-*.log"))

	j.event("PRF_START branch=%s phase=%s step=%s pipeline=%s", j.branch, p.name, step, j.pipeline)
	round, fixes := len(saved), 0
	end := func(code int, note string) int {
		j.event("PRF_PHASE branch=%s phase=%s step=%s exit=%d rounds=%d%s", j.branch, p.name, step, code, round-len(saved), note)
		return code
	}
	// A short TMPDIR of its own, as the command would have if you ran it:
	// the job's folder can be too deep for a test's unix socket.
	tmp, err := os.MkdirTemp("", "prf-check-")
	if err != nil {
		logf("[%s] %s: making a TMPDIR for the command: %v", step, p.name, err)
		return end(exitSetup, "")
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	run := checkRun{tmp: tmp}
	if run.sandbox, err = j.checkSandbox(tmp); err != nil {
		logf("[%s] %s: %v", step, p.name, err)
		return end(exitSetup, "")
	}
	for {
		round++
		j.setState(func(s *jobState) {
			s.State, s.Stage, s.Activity, s.ReviewURL = stateRunning, p.name, fmt.Sprintf("round %d: %s", round, oneLine(command, 80)), ""
		})
		logf("[%s] %s: round %d: running %s", step, p.name, round, command)
		start := time.Now()
		out, code, err := j.runCommand(command, p.timeout, run)
		logFile := filepath.Join(dir, fmt.Sprintf("round-%d.log", round))
		if werr := writeFile(logFile, out); werr != nil {
			logf("saving the output: %v", werr)
		}
		timedOut := errors.Is(err, context.DeadlineExceeded)
		switch {
		case errors.Is(err, errInterrupted):
			logf("[%s] %s: interrupted while running %s", step, p.name, command)
			return end(130, "")
		case err != nil && !timedOut:
			logf("[%s] %s: running %s: %v", step, p.name, command, err)
			return end(exitSetup, "")
		}
		note := ""
		if timedOut {
			note = " stopped=timeout"
		}
		j.event("PRF_CHECK branch=%s phase=%s step=%s round=%d exit=%d%s", j.branch, p.name, step, round, code, note)
		took := shortDuration(time.Since(start))
		if code == 0 {
			logf("[%s] %s: passed in %s", step, p.name, took)
			return end(0, "")
		}
		if timedOut {
			logf("[%s] %s: stopped after %s: its %s timeout", step, p.name, took, p.timeout)
		} else {
			logf("[%s] %s: failed (exit %d) after %s; the output is in %s", step, p.name, code, took, logFile)
		}
		if fixes == p.maxRounds {
			logf("[%s] %s: stopped: still failing after %d fixes; prf resume %s/%s carries on", step, p.name, fixes, j.name, filepath.Base(j.jobDir))
			return end(exitCheck, " stopped=max_rounds")
		}
		fixes++
		full := ""
		if j.tmp != "" { // where the fix role can read it
			full = filepath.Join(j.tmp, fmt.Sprintf("check-%s-round-%d.log", n, round))
			if err := writeFile(full, out); err != nil {
				logf("saving the output: %v", err)
				full = ""
			}
		}
		data.Review = renderFailure(command, code, timedOut, p.timeout, out, full)
		if err := writeFile(filepath.Join(dir, fmt.Sprintf("round-%d.md", round)), data.Review); err != nil {
			logf("saving the failure: %v", err)
		}
		if code := j.runClaude(p, p.fixRole, step, model, data, false); code != 0 {
			return end(code, "")
		}
	}
}

// runCommand runs command with sh in the checkout, with the steps'
// environment and run.tmp as TMPDIR, in srt if run.sandbox is set, and
// returns its combined output and exit code (128+n if signal n killed it);
// the output also goes to the log. When it ends, anything it left running is
// killed. It stops the command, and everything it started, at timeout (0 for
// none), returning context.DeadlineExceeded, or when prf is interrupted,
// returning errInterrupted: SIGTERM first, then SIGKILL after commandGrace.
// Any other error means it didn't start.
func (j *job) runCommand(command string, timeout time.Duration, run checkRun) (string, int, error) {
	ctx, cancel := context.WithCancel(context.Background())
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), timeout)
	}
	defer cancel()
	var out bytes.Buffer
	w := io.Writer(&out)
	if j.feed != nil {
		w = io.MultiWriter(&out, j.feed)
	}
	env := append(append([]string{}, j.env...), "PRF_CHECK="+command, "PRF_CHECK_TMP="+run.tmp)
	if j.env == nil {
		env = append(os.Environ(), env...)
	}
	// srt keeps a socket in TMPDIR, so it gets yours, and the command gets
	// run.tmp. Either way sh runs the command: srt's -c would use bash.
	name, args := "sh", []string{"-c", `TMPDIR="$PRF_CHECK_TMP" exec sh -c "$PRF_CHECK"`}
	if run.sandbox != "" {
		name, args = srtBinary, []string{"-s", run.sandbox, "-c", args[1]}
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = j.dir, env, w, w
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // its own group, so stopping it stops its children
	var stopped atomic.Bool
	cmd.Cancel = func() error {
		stopped.Store(true)
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	cmd.WaitDelay = commandGrace // then Wait kills it, or stops waiting for what it left holding its output
	if err := cmd.Start(); err != nil {
		return "", 0, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	interrupted := false
	var err error
	select {
	case err = <-done:
	case <-j.sigs:
		interrupted = true
		cancel()
		err = <-done
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // whatever it left running
	switch {
	case interrupted:
		return out.String(), 130, errInterrupted
	case stopped.Load() && errors.Is(ctx.Err(), context.DeadlineExceeded):
		return out.String(), 124, ctx.Err() // 124, as timeout(1) exits
	case cmd.ProcessState == nil:
		return out.String(), 0, err
	}
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return out.String(), 128 + int(ws.Signal()), nil
	}
	return out.String(), cmd.ProcessState.ExitCode(), nil // even if Wait gave up on its leftovers' output
}

// renderFailure is a failed check as its fix role reads it, in {{.Review}}:
// the failures in its output, or the end of the output, and where the full
// output is (full, if it was saved).
func renderFailure(command string, code int, timedOut bool, timeout time.Duration, out, full string) string {
	var b strings.Builder
	switch {
	case timedOut:
		fmt.Fprintf(&b, "`%s` didn't finish within %s, so prf stopped it.\n", command, timeout)
	case code > 128:
		fmt.Fprintf(&b, "`%s` was killed by signal %d (exit code %d).\n", command, code-128, code)
	default:
		fmt.Fprintf(&b, "`%s` failed with exit code %d.\n", command, code)
	}
	ex, ok := excerpt(out)
	if !ok || timedOut { // a timeout's clue is what it was doing last
		writeOutput(&b, out)
	} else {
		fmt.Fprintf(&b, "\nThe failures in its output:\n\n````\n%s\n````\n", ex)
		// The excerpt can miss a failure that isn't in Go's formats, so
		// add what the end of the output says that it doesn't.
		if tail := unseenTail(out, ex); tail != "" {
			fmt.Fprintf(&b, "\nThe end of its output:\n\n````\n%s\n````\n", tail)
		}
	}
	if full != "" {
		fmt.Fprintf(&b, "\nThe full output is in %s.\n", full)
	}
	return b.String()
}

// unseenTail is the last ten lines of out that ex doesn't already show,
// leaving out go test's lines for passing packages and its bare FAIL and
// PASS: "" if that's all they are.
func unseenTail(out, ex string) string {
	tail, _ := tailOf(strings.TrimRight(out, "\n"), 10, 2000)
	shown := map[string]bool{}
	for _, l := range strings.Split(ex, "\n") {
		shown[l] = true
	}
	for _, l := range strings.Split(tail, "\n") {
		if !shown[l] && l != "FAIL" && l != "PASS" && strings.TrimSpace(l) != "" && !strings.HasPrefix(l, "ok ") {
			return tail
		}
	}
	return ""
}

// srtBinary is Anthropic's sandbox runtime, which runs a check gate's
// command in a sandboxed job.
var srtBinary = "srt"

// srtSettings is srt's settings file.
type srtSettings struct {
	Filesystem struct {
		AllowWrite []string `json:"allowWrite"`
		DenyWrite  []string `json:"denyWrite"`
		DenyRead   []string `json:"denyRead"`
	} `json:"filesystem"`
	Network struct {
		AllowedDomains      []string `json:"allowedDomains"`
		DeniedDomains       []string `json:"deniedDomains"`
		AllowLocalBinding   bool     `json:"allowLocalBinding,omitempty"`
		AllowUnixSockets    []string `json:"allowUnixSockets,omitempty"`
		AllowAllUnixSockets bool     `json:"allowAllUnixSockets,omitempty"`
	} `json:"network"`
	EnableWeakerNetworkIsolation bool `json:"enableWeakerNetworkIsolation,omitempty"`
}

// checkSandbox returns the srt settings a check gate's command runs with:
// the limits of the sandbox in the job's Claude settings, with its read and
// edit denials, and the checkout, the job's tmp folder and tmp writable. The
// checkout's Claude settings stay unwritable, so the command can't plant a
// hook for the next step. It returns "" when Claude runs unsandboxed, and
// fails when srt isn't installed, rather than run the command without the
// sandbox Claude has.
func (j *job) checkSandbox(tmp string) (string, error) {
	if j.settings == "" {
		return "", nil
	}
	b, err := os.ReadFile(j.settings)
	if err != nil {
		return "", fmt.Errorf("reading the Claude settings: %w", err)
	}
	var claude struct {
		Sandbox *struct {
			Enabled                      bool               `json:"enabled"`
			EnableWeakerNetworkIsolation bool               `json:"enableWeakerNetworkIsolation"`
			Network                      networkSettings    `json:"network"`
			Filesystem                   filesystemSettings `json:"filesystem"`
		} `json:"sandbox"`
		Permissions permissionsSettings `json:"permissions"`
	}
	if err := json.Unmarshal(b, &claude); err != nil {
		return "", fmt.Errorf("reading %s: %w", j.settings, err)
	}
	if claude.Sandbox == nil || !claude.Sandbox.Enabled {
		return "", nil
	}
	if _, err := exec.LookPath(srtBinary); err != nil {
		return "", fmt.Errorf("the job is sandboxed, so @%s runs its command in the same sandbox, with srt, and srt isn't installed: "+
			"npm install -g @anthropic-ai/sandbox-runtime (or run the job with --sandbox=false)", gateCheck)
	}
	var s srtSettings
	fs := claude.Sandbox.Filesystem
	s.Filesystem.AllowWrite = append(append([]string{j.dir, j.tmp, tmp}, j.writeDirs...), fs.AllowWrite...)
	s.Filesystem.DenyWrite = append([]string{filepath.Join(j.dir, ".claude"), filepath.Join(j.dir, ".mcp.json")}, fs.DenyWrite...)
	s.Filesystem.DenyRead = append([]string{}, fs.DenyRead...)
	for _, rule := range claude.Permissions.Deny { // Claude's sandbox enforces these too
		if path, ok := permissionPath(rule, "Read"); ok {
			s.Filesystem.DenyRead = append(s.Filesystem.DenyRead, path)
		} else if path, ok := permissionPath(rule, "Edit"); ok {
			s.Filesystem.DenyWrite = append(s.Filesystem.DenyWrite, path)
		}
	}
	s.Network.AllowedDomains = append([]string{}, claude.Sandbox.Network.AllowedDomains...)
	s.Network.DeniedDomains = []string{}
	net := claude.Sandbox.Network // a test server's localhost socket is as allowed here as in Claude's sandbox
	s.Network.AllowLocalBinding, s.Network.AllowUnixSockets, s.Network.AllowAllUnixSockets = net.AllowLocalBinding, net.AllowUnixSockets, net.AllowAllUnixSockets
	s.EnableWeakerNetworkIsolation = claude.Sandbox.EnableWeakerNetworkIsolation
	out, _ := json.MarshalIndent(s, "", "  ")
	path := filepath.Join(j.jobDir, "check-sandbox.json") // outside what the command may write
	return path, writeFile(path, string(append(out, '\n')))
}

// permissionPath returns the path of a permission rule for tool, such as
// Read(~/.ssh/**), without a trailing /** or /*.
func permissionPath(rule, tool string) (string, bool) {
	path, ok := strings.CutPrefix(rule, tool+"(")
	if !ok {
		return "", false
	}
	if path, ok = strings.CutSuffix(path, ")"); !ok {
		return "", false
	}
	path = strings.TrimSuffix(strings.TrimSuffix(path, "/**"), "/*")
	return path, path != ""
}

// writeOutput adds a command's output to b, fenced, or its end if it's long.
func writeOutput(b *strings.Builder, out string) {
	out = strings.TrimRight(out, "\n")
	if out == "" {
		b.WriteString("\nIt printed nothing.\n")
		return
	}
	tail, cut := tailOf(out, failureLines, failureBytes)
	if cut {
		b.WriteString("\nThe end of its output:\n")
	} else {
		b.WriteString("\nIts output:\n")
	}
	fmt.Fprintf(b, "\n````\n%s\n````\n", tail)
}

// tailOf returns the end of s: its last lines, at most maxLines of them and
// maxBytes long, and whether that left anything out.
func tailOf(s string, maxLines, maxBytes int) (string, bool) {
	cut := false
	if len(s) > maxBytes {
		s, cut = s[len(s)-maxBytes:], true
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:] // from a whole line
		}
	}
	if lines := strings.Split(s, "\n"); len(lines) > maxLines {
		s, cut = strings.Join(lines[len(lines)-maxLines:], "\n"), true
	}
	return s, cut
}
