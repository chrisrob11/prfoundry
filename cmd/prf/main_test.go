package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chrisrob11/prfoundry/internal/config"
	"github.com/chrisrob11/prfoundry/internal/jobenv"
	"github.com/chrisrob11/prfoundry/internal/pipeline"
	"github.com/chrisrob11/prfoundry/internal/sandbox"
	"github.com/chrisrob11/prfoundry/internal/state"
	"github.com/urfave/cli/v3"
)

// fakeClaude records each call as "cwd\nargs\n---\n" in the log, and the
// mise trust paths it saw in <log>.mise. It speaks stream-json: a tool call,
// a message, and a result costing $0.05. A prompt containing FAIL exits 1;
// ERROR reports an error but exits 0; BUDGET stops at its budget; SLOW takes
// half a second. A prompt containing ASK writes a question to $FAKE_ASK_PATH
// ($PRF_TMP/stop.md if unset) while the count in $FAKE_ASKS is above
// zero, and counts it down; one containing ANSWER deletes the question; one
// containing COMMIT commits a change to fixed.txt; DIRTY leaves seven
// untracked files. One containing $FAKE_FAIL_ON, if set, fails like FAIL.
const fakeClaude = `#!/bin/sh
printf '%s\n%s\n---\n' "$(pwd)" "$*" >> "$FAKE_LOG"
FAKE_ASK_PATH=${FAKE_ASK_PATH:-$PRF_TMP/stop.md}
echo "$MISE_TRUSTED_CONFIG_PATHS" >> "$FAKE_LOG.mise"
case "$2" in *ASK*)
  n=$(cat "$FAKE_ASKS" 2>/dev/null || echo 0)
  if [ "$n" -gt 0 ]; then
    echo $((n - 1)) > "$FAKE_ASKS"
    mkdir -p "$(dirname "$FAKE_ASK_PATH")"
    printf '# Which way?\n\nSee README.md:1.\n\n- (a) left (Recommended)\n- (b) right\n' > "$FAKE_ASK_PATH"
  fi;;
esac
case "$2" in *ANSWER*) rm -f "$FAKE_ASK_PATH";; esac
case "$2" in *LIMIT*)
  n=$(cat "$FAKE_LIMITS" 2>/dev/null || echo 0)
  if [ "$n" -gt 0 ]; then
    echo $((n - 1)) > "$FAKE_LIMITS"
    printf '%s\n' '{"type":"result","is_error":true,"result":"You'"'"'ve hit your 5-hour limit · resets 3pm (UTC)","num_turns":1,"total_cost_usd":0}'; exit 1
  fi;;
esac
case "$2" in *COMMIT*) echo x >> fixed.txt && git add fixed.txt && git commit -qm "fix: fake";; esac
case "$2" in *DIRTY*) for i in 1 2 3 4 5 6 7; do echo x > dirty$i.txt; done;; esac
if [ -n "$FAKE_FAIL_ON" ]; then case "$2" in *"$FAKE_FAIL_ON"*)
  printf '%s\n' '{"type":"result","is_error":true,"result":"it broke","num_turns":2,"total_cost_usd":0.01}'; exit 1;;
esac; fi
printf '%s\n' '{"type":"system","subtype":"init"}'
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}},{"type":"text","text":"Tests pass.\nMore detail."}]}}'
case "$2" in *SLOW*) sleep 0.5;; esac
case "$2" in
  *BUDGET*) printf '%s\n' '{"type":"result","subtype":"error_max_budget_usd","is_error":true,"num_turns":4,"total_cost_usd":0.30,"errors":["Reached maximum budget"]}'; exit 1;;
  *FAIL*) printf '%s\n' '{"type":"result","is_error":true,"result":"it broke","num_turns":2,"total_cost_usd":0.01}'; exit 1;;
  *ERROR*) printf '%s\n' '{"type":"result","is_error":true,"result":"gave up","num_turns":2,"total_cost_usd":0.01}';;
  *) printf '%s\n' '{"type":"result","is_error":false,"result":"did the thing","num_turns":3,"total_cost_usd":0.05,"usage":{"cache_read_input_tokens":90000}}';;
esac
`

type env struct {
	origin string // repo to clone
	home   string // prf home
	roles  string // shared roles dir
	log    string // fake claude call log
	root   string
}

// newEnv sets up an origin repo, a fake claude, the shared roles, and a
// conf.json whose fields are the basics plus conf (JSON object members, e.g.
// `"sandbox": false`); {{origin}} in conf becomes the origin repo's path.
// The working directory moves to a temp dir.
// removeFile removes path, which may not exist, failing t on any other error.
func removeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
}

func newEnv(t *testing.T, conf string) env {
	t.Helper()
	e := env{origin: newOrigin(t), root: t.TempDir()}
	t.Chdir(e.root)
	e.home = filepath.Join(e.root, "home")
	e.roles = filepath.Join(e.home, "roles")
	e.log = filepath.Join(e.root, "claude.log")
	t.Setenv("FAKE_LOG", e.log)
	for _, k := range []string{"PRF_WORKDIR", "PRF_HOME", "CLAUDE_BIN"} {
		t.Setenv(k, "") // registers the restore; cli reads an empty variable as set
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}

	claude := filepath.Join(e.root, "claude")
	writeFileT(t, claude, fakeClaude)
	if err := os.Chmod(claude, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"implement", "test", "review"} {
		writeRole(t, e.roles, name, name+" on {{.Branch}}{{if .PR}} pr={{.PR}}{{end}}")
	}
	if conf != "" {
		conf = ",\n" + strings.ReplaceAll(conf, "{{origin}}", e.origin)
	}
	writeFileT(t, filepath.Join(e.home, config.FileName), `{"claude": "`+claude+`", "workdir": "`+filepath.Join(e.root, "work")+`"`+conf+"}\n")
	return e
}

// calls returns each recorded claude call as {cwd, args}.
func (e env) calls(t *testing.T) [][2]string {
	t.Helper()
	b, err := os.ReadFile(e.log)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls [][2]string
	for rec := range strings.SplitSeq(strings.TrimSuffix(string(b), "---\n"), "---\n") {
		cwd, args, _ := strings.Cut(rec, "\n")
		calls = append(calls, [2]string{cwd, strings.TrimSuffix(args, "\n")})
	}
	return calls
}

func (e env) run(t *testing.T, args ...string) int {
	t.Helper()
	return runPrf(t, append([]string{"--home", e.home}, args...)...)
}

// runPrf runs the app in-process and returns its exit code.
func runPrf(t *testing.T, args ...string) int {
	t.Helper()
	code, _ := runPrfIO(t, "", args...)
	return code
}

// runPrfIO runs the app in-process with stdin, returning its exit code and stdout.
func runPrfIO(t *testing.T, stdin string, args ...string) (int, string) {
	t.Helper()
	var out strings.Builder
	app := newApp()
	app.Reader, app.Writer = strings.NewReader(stdin), &out
	err := app.Run(context.Background(), append([]string{"prf"}, args...))
	return exitCode(t, err), out.String()
}

func exitCode(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	if ec, ok := errors.AsType[cli.ExitCoder](err); ok {
		return ec.ExitCode()
	}
	t.Logf("error: %v", err)
	return exitUsage
}

func TestRunWithConfigDefaults(t *testing.T) {
	plans := t.TempDir()
	writeFileT(t, filepath.Join(plans, "login.md"), "# login")
	e := newEnv(t, `"pr_dir": "`+plans+`", "branch_prefix": "prf/", "claude_args": ["--model", "sonnet"],
		"repos": {"app": "{{origin}}"}`)

	if code := e.run(t, "run", "--repo", "app", "--pr", "login", "--", "--verbose"); code != 0 {
		t.Fatalf("exit %d", code)
	}

	jobDir := filepath.Join(e.root, "work", "app", "login")
	clone := filepath.Join(jobDir, "repo")
	calls := e.calls(t)
	if len(calls) != 3 {
		t.Fatalf("got %d claude calls, want 3: %v", len(calls), calls)
	}
	wantPR := filepath.Join(plans, "login.md")
	for i, role := range []string{"implement", "test", "review"} {
		cwd, args := calls[i][0], calls[i][1]
		if !sameDir(cwd, clone) {
			t.Errorf("%s ran in %s, want %s", role, cwd, clone)
		}
		if !strings.Contains(args, role+" on prf/login pr="+wantPR) {
			t.Errorf("%s prompt wrong: %s", role, args)
		}
		if !strings.HasSuffix(args, "--model sonnet --verbose") {
			t.Errorf("%s: config claude_args then -- args not passed last: %s", role, args)
		}
	}
	if got := mustGit(t, clone, "branch", "--show-current"); got != "prf/login" {
		t.Errorf("branch = %q", got)
	}

	status, err := os.ReadFile(filepath.Join(jobDir, "status"))
	if err != nil {
		t.Fatal(err)
	}
	want := `PRF_START branch=prf/login phase=implement step=1/3 pipeline=default
PRF_PHASE branch=prf/login phase=implement step=1/3 exit=0 error=false turns=3 cost=0.05
PRF_START branch=prf/login phase=test step=2/3 pipeline=default
PRF_PHASE branch=prf/login phase=test step=2/3 exit=0 error=false turns=3 cost=0.05
PRF_START branch=prf/login phase=review step=3/3 pipeline=default
PRF_PHASE branch=prf/login phase=review step=3/3 exit=0 error=false turns=3 cost=0.05
PRF_DONE branch=prf/login phase=review exit=0 dir=` + clone + "\n"
	if string(status) != want {
		t.Errorf("status =\n%s\nwant\n%s", status, want)
	}
	log, err := os.ReadFile(filepath.Join(jobDir, "claude.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(log), "=== prf phase") != 3 {
		t.Errorf("log should have 3 phase headers:\n%s", log)
	}
}

func TestRunCloneExists(t *testing.T) {
	e := newEnv(t, "")
	args := []string{"run", "--repo", e.origin, "--branch", "b", "--prompt", "x", "--role", "review"}
	if code := e.run(t, args...); code != 0 {
		t.Fatalf("first run: exit %d", code)
	}
	jobDir := filepath.Join(e.root, "work", "origin", "b")
	before, _ := os.ReadFile(filepath.Join(jobDir, "status"))
	beforeState, _ := os.ReadFile(filepath.Join(jobDir, state.FileName))
	if code := e.run(t, args...); code != exitSetup {
		t.Fatalf("second run: exit %d, want %d", code, exitSetup)
	}
	after, _ := os.ReadFile(filepath.Join(jobDir, "status"))
	afterState, _ := os.ReadFile(filepath.Join(jobDir, state.FileName))
	if string(after) != string(before) || string(afterState) != string(beforeState) {
		t.Error("refusing to rerun should leave the earlier job's status and state alone")
	}
}

func TestRunStopsAtFailingPhase(t *testing.T) {
	e := newEnv(t, "")
	writeRole(t, e.roles, "test", "FAIL")
	status := filepath.Join(e.root, "st")

	code := e.run(t, "run", "--repo", e.origin, "--branch", "b", "--prompt", "x", "--status", status)
	if code != 1 {
		t.Fatalf("exit %d, want 1 from the failing phase", code)
	}
	if n := len(e.calls(t)); n != 2 {
		t.Errorf("got %d claude calls, want 2 (review must not run)", n)
	}
	b, _ := os.ReadFile(status)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if last := lines[len(lines)-1]; !strings.HasPrefix(last, "PRF_DONE branch=b phase=test exit=1 ") {
		t.Errorf("last status line = %q", last)
	}
}

func TestRepoRolesOverrideShared(t *testing.T) {
	e := newEnv(t, "")
	writeRole(t, filepath.Join(e.home, "origin", "roles"), "review", "repo review")

	if code := e.run(t, "run", "--repo", e.origin, "--branch", "b", "--role", "review,test"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	calls := e.calls(t)
	if len(calls) != 2 || !strings.Contains(calls[0][1], "repo review") || !strings.Contains(calls[1][1], "test on b") {
		t.Errorf("want the repo's review and the shared test: %v", calls)
	}
}

// apply --repo with --dir resumes an earlier run's clone as that repo.
func TestApplyResumesRunClone(t *testing.T) {
	e := newEnv(t, `"repos": {"app": "{{origin}}"}`)
	writeRole(t, filepath.Join(e.home, "app", "roles"), "review", "app review")
	pr := filepath.Join(e.root, "login.md")
	writeFileT(t, pr, "# login")
	writeRole(t, e.roles, "test", "FAIL")

	if code := e.run(t, "run", "--repo", "app", "--branch", "b", "--pr", pr); code != 1 {
		t.Fatalf("run: exit %d, want 1 from the failing test role", code)
	}
	writeRole(t, e.roles, "test", "test on {{.Branch}}")
	removeFile(t, e.log)

	jobDir := filepath.Join(e.root, "work", "app", "login")
	if code := e.run(t, "apply", "--repo", "app", "--dir", filepath.Join(jobDir, "repo"), "--pr", pr, "--role", "test,review"); code != 0 {
		t.Fatalf("apply: exit %d", code)
	}
	calls := e.calls(t)
	if len(calls) != 2 || !strings.Contains(calls[1][1], "app review") {
		t.Errorf("resume should use app's roles: %v", calls)
	}
	b, _ := os.ReadFile(filepath.Join(jobDir, "status"))
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if last := lines[len(lines)-1]; !strings.HasPrefix(last, "PRF_DONE branch=b phase=review exit=0 ") {
		t.Errorf("resume should append to the same job status; last line %q", last)
	}
}

func TestRunWithPipelines(t *testing.T) {
	e := newEnv(t, `"repos": {"app": "{{origin}}"}`)
	writeFileT(t, filepath.Join(e.home, "app", pipeline.FileName), `{
  "default": "short",
  "pipelines": {"short": ["review"], "long": ["test", "implement", "review"]}
}`)

	tests := []struct {
		name string
		args []string
		want []string
	}{
		{name: "repo default", args: []string{"--repo", "app", "--branch", "a"}, want: []string{"review"}},
		{name: "named pipeline", args: []string{"--repo", "app", "--branch", "b", "--pipeline", "long"}, want: []string{"test", "implement", "review"}},
		{name: "roles flag", args: []string{"--repo", "app", "--branch", "c", "--role", "test"}, want: []string{"test"}},
		{name: "built-in default without pipeline.json", args: []string{"--repo", e.origin, "--branch", "d"}, want: []string{"implement", "test", "review"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			removeFile(t, e.log)
			if code := e.run(t, append([]string{"run", "--prompt", "x"}, tt.args...)...); code != 0 {
				t.Fatalf("exit %d", code)
			}
			var got []string
			for _, call := range e.calls(t) {
				got = append(got, strings.Fields(strings.TrimPrefix(call[1], "-p "))[0])
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("ran %v, want %v", got, tt.want)
			}
		})
	}

	// apply only runs a pipeline when asked.
	repo := filepath.Join(e.root, "checkout")
	mustGit(t, "", "clone", "-q", e.origin, repo)
	removeFile(t, e.log)
	if code := e.run(t, "apply", "--dir", repo, "--prompt", "x"); code != 0 {
		t.Fatalf("apply: exit %d", code)
	}
	if calls := e.calls(t); len(calls) != 1 || !strings.HasPrefix(calls[0][1], "-p x\n\n## Codebase context") {
		t.Errorf("apply without --pipeline should send the prompt alone: %v", calls)
	}
}

func TestApply(t *testing.T) {
	e := newEnv(t, "")
	repo := filepath.Join(e.root, "checkout")
	mustGit(t, "", "clone", "-q", e.origin, repo)

	if code := e.run(t, "apply", "--dir", repo, "--branch", "feat/a", "--prompt", "just this"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	calls := e.calls(t)
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	if !sameDir(calls[0][0], repo) {
		t.Errorf("ran in %s, want %s", calls[0][0], repo)
	}
	settings := filepath.Join(e.home, sandbox.SettingsFile)
	if !strings.HasPrefix(calls[0][1], "-p just this\n\n## Codebase context") || !strings.HasSuffix(calls[0][1], "--settings "+settings) {
		t.Errorf("args = %q, want the prompt, then the codebase context, then --settings %s", calls[0][1], settings)
	}
	if got := mustGit(t, repo, "branch", "--show-current"); got != "feat/a" {
		t.Errorf("branch = %q, want feat/a", got)
	}
	jobDir := filepath.Join(e.root, "work", "checkout", "feat-a")
	for _, f := range []string{"status", "claude.log"} {
		if _, err := os.Stat(filepath.Join(jobDir, f)); err != nil {
			t.Errorf("job folder missing %s: %v", f, err)
		}
	}
}

// run trusts mise configs in its own clones; apply leaves your checkout alone.
func TestMiseTrust(t *testing.T) {
	e := newEnv(t, "")
	t.Setenv("MISE_TRUSTED_CONFIG_PATHS", "") // miseEnv treats empty as unset
	repo := filepath.Join(e.root, "checkout")
	mustGit(t, "", "clone", "-q", e.origin, repo)

	if code := e.run(t, "run", "--repo", e.origin, "--branch", "b", "--role", "review"); code != 0 {
		t.Fatalf("run: exit %d", code)
	}
	if code := e.run(t, "apply", "--dir", repo, "--prompt", "x"); code != 0 {
		t.Fatalf("apply: exit %d", code)
	}
	// apply in run's clone trusts the workdir, as resume does.
	if code := e.run(t, "apply", "--repo", "origin", "--dir", filepath.Join(e.root, "work", "origin", "b", "repo"), "--prompt", "x"); code != 0 {
		t.Fatalf("apply in the clone: exit %d", code)
	}
	b, _ := os.ReadFile(e.log + ".mise")
	work := filepath.Join(e.root, "work")
	if got, want := string(b), work+"\n\n"+work+"\n"; got != want {
		t.Errorf("trusted paths seen by claude = %q, want %q (run, apply in a checkout, apply in the clone)", got, want)
	}
}

func TestEmptyPrompt(t *testing.T) {
	e := newEnv(t, "")
	writeRole(t, e.roles, "raw", "{{.Task}}")
	pr := filepath.Join(e.root, "spec.md")
	writeFileT(t, pr, "")

	if code := e.run(t, "run", "--repo", e.origin, "--pr", pr, "--role", "raw"); code != exitUsage {
		t.Fatalf("exit %d, want %d", code, exitUsage)
	}
	if calls := e.calls(t); calls != nil {
		t.Errorf("claude ran with an empty prompt: %v", calls)
	}
}

func TestCodebaseContext(t *testing.T) {
	e := newEnv(t, `"repos": {"app": "{{origin}}"}`)
	run := func(branch string, extra ...string) int {
		removeFile(t, e.log)
		return e.run(t, append([]string{"run", "--repo", "app", "--branch", branch, "--prompt", "x", "--role", "review"}, extra...)...)
	}
	contextDir := func(branch string) string { return filepath.Join(e.root, "work", "app", branch, "context") }

	if code := run("map"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	index := filepath.Join(contextDir("map"), "index.md")
	if _, err := os.Stat(index); err != nil {
		t.Fatalf("no codebase map: %v", err)
	}
	args := e.calls(t)[0][1]
	if !strings.Contains(args, "## Codebase context") || !strings.Contains(args, "- "+index) || !strings.Contains(args, "--add-dir "+contextDir("map")) {
		t.Errorf("claude not pointed at the map: %s", args)
	}

	// Setup commands run in the clone with PRF_CONTEXT, and their files are listed too.
	writeFileT(t, filepath.Join(e.home, "app", pipeline.FileName), `{"pipelines": {}, "index": false,
		"setup": ["git rev-parse --abbrev-ref HEAD > $PRF_CONTEXT/branch.txt"]}`)
	if code := run("setup"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if b, _ := os.ReadFile(filepath.Join(contextDir("setup"), "branch.txt")); string(b) != "setup\n" {
		t.Errorf("setup output = %q, want the clone's branch", b)
	}
	if _, err := os.Stat(filepath.Join(contextDir("setup"), "index.md")); err == nil {
		t.Error(`"index": false still built the map`)
	}
	if args := e.calls(t)[0][1]; !strings.Contains(args, "branch.txt") {
		t.Errorf("setup file not listed: %s", args)
	}

	// A failing setup command stops the job before Claude runs.
	writeFileT(t, filepath.Join(e.home, "app", pipeline.FileName), `{"pipelines": {}, "setup": ["exit 7"]}`)
	if code := run("broken"); code != exitSetup {
		t.Errorf("failing setup: exit %d, want %d", code, exitSetup)
	}
	if calls := e.calls(t); calls != nil {
		t.Errorf("claude ran after a failed setup: %v", calls)
	}
}

func TestSandboxSwitch(t *testing.T) {
	tests := []struct {
		name, config string
		flag         []string
		want         bool
	}{
		{name: "on by default", want: true},
		{name: "flag off", flag: []string{"--sandbox=false"}, want: false},
		{name: "config off", config: `"sandbox": false`, want: false},
		{name: "flag beats config", config: `"sandbox": false`, flag: []string{"--sandbox"}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, tt.config)
			args := append([]string{"run", "--repo", e.origin, "--branch", "b", "--role", "review"}, tt.flag...)
			if code := e.run(t, args...); code != 0 {
				t.Fatalf("exit %d", code)
			}
			calls := e.calls(t)
			if got := strings.Contains(calls[0][1], "--settings "); got != tt.want {
				t.Errorf("--settings passed = %v, want %v: %s", got, tt.want, calls[0][1])
			}
		})
	}
}

// --claude given as a relative path means relative to where prf runs.
func TestRelativeClaudePath(t *testing.T) {
	e := newEnv(t, "")
	writeFileT(t, filepath.Join(e.root, "bin", "fake"), fakeClaude)
	if err := os.Chmod(filepath.Join(e.root, "bin", "fake"), 0o755); err != nil {
		t.Fatal(err)
	}
	captureStderr(t)
	if code := e.run(t, "run", "--repo", e.origin, "--branch", "b", "--prompt", "x", "--role", "review", "--claude", "bin/fake"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if n := len(e.calls(t)); n != 1 {
		t.Errorf("got %d calls through bin/fake, want 1", n)
	}
}

func TestUsageErrors(t *testing.T) {
	e := newEnv(t, `"repos": {"nopath": "git@example.com:x.git"}`)

	tests := []struct {
		name string
		args []string
	}{
		{"run without repo", []string{"run", "--branch", "b", "--prompt", "x"}},
		{"run without branch or pr", []string{"run", "--repo", e.origin, "--prompt", "x"}},
		{"unknown role", []string{"run", "--repo", e.origin, "--branch", "b", "--prompt", "x", "--role", "nope"}},
		{"missing pr file", []string{"run", "--repo", e.origin, "--pr", "nope.md"}},
		{"apply outside a repo", []string{"apply", "--dir", t.TempDir(), "--prompt", "x"}},
		{"apply alias without path or --dir", []string{"apply", "--repo", "nopath", "--prompt", "x"}},
		{"apply with nothing to do", []string{"apply", "--dir", e.origin}},
		{"unknown pipeline", []string{"run", "--repo", e.origin, "--branch", "b", "--prompt", "x", "--pipeline", "nope"}},
		{"role and pipeline", []string{"run", "--repo", e.origin, "--branch", "b", "--prompt", "x", "--role", "test", "--pipeline", "default"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if code := e.run(t, tt.args...); code != exitUsage {
				t.Errorf("exit %d, want %d", code, exitUsage)
			}
		})
	}
	if calls := e.calls(t); calls != nil {
		t.Errorf("claude ran on a usage error: %v", calls)
	}
	if _, err := os.Stat(filepath.Join(e.root, "work")); err == nil {
		t.Error("a usage error still cloned the repo")
	}
}

func TestResolvePR(t *testing.T) {
	dir := t.TempDir()
	writeFileT(t, filepath.Join(dir, "login.md"), "")
	writeFileT(t, filepath.Join(dir, "notes"), "")
	t.Chdir(t.TempDir())
	writeFileT(t, "here.md", "")
	here, _ := filepath.Abs("here.md")

	tests := []struct {
		pr, prDir, want, wantErr string
	}{
		{pr: "", want: ""},
		{pr: "here.md", prDir: dir, want: here},
		{pr: "login", prDir: dir, want: filepath.Join(dir, "login.md")},
		{pr: "login.md", prDir: dir, want: filepath.Join(dir, "login.md")},
		{pr: "notes", prDir: dir, want: filepath.Join(dir, "notes")},
		{pr: "login", wantErr: "PR file not found"},
		{pr: filepath.Join("/nonexistent", "login"), prDir: dir, wantErr: "PR file not found"},
	}
	for _, tt := range tests {
		t.Run(tt.pr, func(t *testing.T) {
			got, err := resolvePR(tt.pr, tt.prDir)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// sameDir compares paths after resolving symlinks (macOS temp dirs live under /private).
func sameDir(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}

// syncBuffer is a bytes.Buffer safe for the heartbeat goroutine to write to.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// captureStderr sends prf's progress output to a buffer for the test.
func captureStderr(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := stderr
	stderr = buf
	t.Cleanup(func() { stderr = prev })
	return buf
}

func TestProgressOutput(t *testing.T) {
	e := newEnv(t, "")
	writeRole(t, e.roles, "test", "SLOW test")
	prev := heartbeatEvery
	heartbeatEvery = 100 * time.Millisecond
	t.Cleanup(func() { heartbeatEvery = prev })

	errOut := captureStderr(t)
	code, out := runPrfIO(t, "", "--home", e.home, "run", "--repo", e.origin, "--branch", "b", "--prompt", "x", "--role", "implement,test")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	progress := errOut.String()
	for _, want := range []string{
		"prf: [1/2] implement: started\n",
		"prf: [1/2] implement: done in ",
		" (3 turns, $0.05)\n",
		"prf: [2/2] test: started\n",
		"prf: [2/2] test: working (",
		"1 tool calls; last: [bash] go test ./...)\n",
		"prf: pipeline roles: 2/2 steps succeeded in ",
		", $0.10\n",
	} {
		if !strings.Contains(progress, want) {
			t.Errorf("progress missing %q", want)
		}
	}
	if strings.Contains(progress, "[claude] Tests pass.") {
		t.Error("Claude's steps should only reach stderr with --verbose")
	}
	if out != "did the thing\ndid the thing\n" {
		t.Errorf("stdout = %q, want each phase's summary", out)
	}

	jobDir := filepath.Join(e.root, "work", "origin", "b")
	log, _ := os.ReadFile(filepath.Join(jobDir, "claude.log"))
	for _, want := range []string{
		"=== prf phase implement (step 1/2, pipeline roles) ===\n  [bash] go test ./...\n  [claude] Tests pass.\n--- implement summary ---\ndid the thing\n",
	} {
		if !strings.Contains(string(log), want) {
			t.Errorf("claude.log missing %q:\n%s", want, log)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(jobDir, "claude.jsonl"))
	if strings.Count(string(raw), `"type":"result"`) != 2 {
		t.Errorf("claude.jsonl should hold both raw streams:\n%s", raw)
	}

	// --verbose puts Claude's steps on stderr too.
	errOut = captureStderr(t)
	if code := e.run(t, "run", "--repo", e.origin, "--branch", "v", "--prompt", "x", "--role", "implement", "-v"); code != 0 {
		t.Fatalf("verbose: exit %d", code)
	}
	if !strings.Contains(errOut.String(), "  [bash] go test ./...\n  [claude] Tests pass.\n") {
		t.Errorf("--verbose should show Claude's steps:\n%s", errOut)
	}
	if strings.Contains(errOut.String(), "did the thing") {
		t.Error("--verbose should leave the summary to stdout, not repeat it on stderr")
	}
}

// A result that reports an error fails the phase even if Claude exits 0.
func TestReportedErrorFailsPhase(t *testing.T) {
	e := newEnv(t, "")
	writeRole(t, e.roles, "test", "ERROR")
	captureStderr(t)
	if code := e.run(t, "run", "--repo", e.origin, "--branch", "b", "--prompt", "x"); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if n := len(e.calls(t)); n != 2 {
		t.Errorf("got %d calls, want 2: review must not run after test reported an error", n)
	}
	b, _ := os.ReadFile(filepath.Join(e.root, "work", "origin", "b", "status"))
	if !strings.Contains(string(b), "phase=test step=2/3 exit=1 error=true turns=2 cost=0.01\n") {
		t.Errorf("status:\n%s", b)
	}
}

// modelsUsed returns the --model given to each claude call, or "" for none.
func modelsUsed(t *testing.T, e env) []string {
	t.Helper()
	var models []string
	for _, call := range e.calls(t) {
		_, after, found := strings.Cut(call[1], " --model ")
		model := ""
		if found {
			model, _, _ = strings.Cut(after, " ")
		}
		models = append(models, model)
	}
	return models
}

func TestStepModels(t *testing.T) {
	e := newEnv(t, `"model": "sonnet", "repos": {"app": "{{origin}}", "plain": "{{origin}}"}`)
	writeFileT(t, filepath.Join(e.home, "app", pipeline.FileName), `{
  "default": "feature",
  "model": "haiku",
  "pipelines": {"feature": [{"role": "implement", "model": "opus"}, "test", {"role": "review", "model": "claude-sonnet-5-5[1m]"}]}
}`)
	errOut := captureStderr(t)

	tests := []struct {
		name string
		args []string
		want []string
	}{
		{name: "step, then repo default", args: []string{"--repo", "app", "--branch", "a"}, want: []string{"opus", "haiku", "claude-sonnet-5-5[1m]"}},
		{name: "--model overrides every step", args: []string{"--repo", "app", "--branch", "b", "--model", "haiku"}, want: []string{"haiku", "haiku", "haiku"}},
		{name: "--role role:model, then repo default", args: []string{"--repo", "app", "--branch", "c", "--role", "test:opus,review"}, want: []string{"opus", "haiku"}},
		{name: "conf.json default without pipeline.json", args: []string{"--repo", "plain", "--branch", "d"}, want: []string{"sonnet", "sonnet", "sonnet"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			removeFile(t, e.log)
			if code := e.run(t, append([]string{"run", "--prompt", "x"}, tt.args...)...); code != 0 {
				t.Fatalf("exit %d", code)
			}
			if got := modelsUsed(t, e); !slices.Equal(got, tt.want) {
				t.Errorf("models = %v, want %v", got, tt.want)
			}
		})
	}

	jobDir := filepath.Join(e.root, "work", "app", "a")
	status, _ := os.ReadFile(filepath.Join(jobDir, "status"))
	if !strings.Contains(string(status), "PRF_START branch=a phase=implement step=1/3 pipeline=feature model=opus\n") {
		t.Errorf("status lines should name the model:\n%s", status)
	}
	if s := readState(t, jobDir); !slices.Equal(s.Models, []string{"opus", "haiku", "claude-sonnet-5-5[1m]"}) {
		t.Errorf("state models = %v", s.Models)
	}
	if !strings.Contains(errOut.String(), "prf: [1/3] implement (opus): started\n") {
		t.Errorf("progress should name the model:\n%s", errOut)
	}
	_, text := runPrfIO(t, "", "--home", e.home, "status", "--repo", "app")
	if !strings.Contains(text, "review (claude-sonnet-5-5[1m])") {
		t.Errorf("prf status should show the last step's model:\n%s", text)
	}

	if code := e.run(t, "run", "--repo", "app", "--branch", "x", "--prompt", "x", "--role", "test:bad model"); code != exitUsage {
		t.Errorf("invalid model in --role: exit %d, want %d", code, exitUsage)
	}
}

func TestPipelineSetWithModels(t *testing.T) {
	h := newHome(t)
	if code, out := prf(t, h, "", "pipeline", "set", "feature", "--repo", "app", "--roles", "implement:opus,test,review:haiku"); code != 0 {
		t.Fatalf("set: %d %s", code, out)
	}
	b, _ := os.ReadFile(filepath.Join(h, "app", pipeline.FileName))
	if !strings.Contains(string(b), `{
        "role": "implement",
        "model": "opus"
      },
      "test",`) {
		t.Errorf("pipeline.json should keep plain roles as strings:\n%s", b)
	}
	if code, out := prf(t, h, "", "pipeline", "show", "feature", "--repo", "app"); code != 0 || out != "implement:opus\ntest\nreview:haiku\n" {
		t.Errorf("show = %d %q", code, out)
	}
	if code, _ := prf(t, h, "", "pipeline", "set", "bad", "--repo", "app", "--roles", "implement:,test"); code != exitUsage {
		t.Errorf("empty model after the colon: exit %d, want %d", code, exitUsage)
	}
}

// budgetsUsed returns the --max-budget-usd given to each claude call, or "".
func budgetsUsed(t *testing.T, e env) []string {
	t.Helper()
	var budgets []string
	for _, call := range e.calls(t) {
		_, after, found := strings.Cut(call[1], " --max-budget-usd ")
		budget := ""
		if found {
			budget, _, _ = strings.Cut(after, " ")
		}
		budgets = append(budgets, budget)
	}
	return budgets
}

func TestCostCaps(t *testing.T) {
	e := newEnv(t, `"max_cost": 10, "repos": {"app": "{{origin}}"}`)
	writeFileT(t, filepath.Join(e.home, "app", pipeline.FileName), `{
  "default": "default",
  "max_step_cost": 1,
  "pipelines": {"default": ["implement", {"role": "test", "max_cost": 2}, "review"]}
}`)
	captureStderr(t)

	tests := []struct {
		name string
		args []string
		want []string
	}{
		{name: "step cap, else the repo's step cap", args: []string{"--branch", "a"}, want: []string{"1", "2", "1"}},
		{name: "--max-step-cost overrides every step", args: []string{"--branch", "b", "--max-step-cost", "0.5"}, want: []string{"0.5", "0.5", "0.5"}},
		{name: "what's left of the job cap, when it's smaller", args: []string{"--branch", "c", "--max-cost", "1.07"}, want: []string{"1", "1.02", "0.97"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			removeFile(t, e.log)
			if code := e.run(t, append([]string{"run", "--repo", "app", "--prompt", "x"}, tt.args...)...); code != 0 {
				t.Fatalf("exit %d", code)
			}
			if got := budgetsUsed(t, e); !slices.Equal(got, tt.want) {
				t.Errorf("budgets = %v, want %v", got, tt.want)
			}
		})
	}

	t.Run("job cap used up stops before the next step", func(t *testing.T) {
		removeFile(t, e.log)
		if code := e.run(t, "run", "--repo", "app", "--branch", "d", "--prompt", "x", "--max-cost", "0.10"); code != exitBudget {
			t.Fatalf("exit %d, want %d", code, exitBudget)
		}
		if n := len(e.calls(t)); n != 2 {
			t.Errorf("got %d calls, want 2: review must not start with nothing left", n)
		}
		b, _ := os.ReadFile(filepath.Join(e.root, "work", "app", "d", "status"))
		if !strings.Contains(string(b), "phase=review step=3/3 exit=5 stopped=max_cost\n") {
			t.Errorf("status:\n%s", b)
		}
	})

	t.Run("a step that hits its budget stops the job", func(t *testing.T) {
		writeRole(t, e.roles, "test", "BUDGET")
		t.Cleanup(func() { writeRole(t, e.roles, "test", "test on {{.Branch}}") })
		removeFile(t, e.log)
		if code := e.run(t, "run", "--repo", "app", "--branch", "f", "--prompt", "x"); code != exitBudget {
			t.Fatalf("exit %d, want %d", code, exitBudget)
		}
		if n := len(e.calls(t)); n != 2 {
			t.Errorf("got %d calls, want 2", n)
		}
		b, _ := os.ReadFile(filepath.Join(e.root, "work", "app", "f", "status"))
		if !strings.Contains(string(b), "phase=test step=2/3 exit=5 error=true turns=4 cost=0.30 stopped=max_cost\n") {
			t.Errorf("status:\n%s", b)
		}
	})
}

// run writes <job>/mise.toml and seeds the job folder before the first step,
// and lets every step write to the folders job_env and job_seed name.
func TestRunJobEnv(t *testing.T) {
	shared := t.TempDir()
	writeFileT(t, filepath.Join(shared, "go-build", "00", "entry"), "warm")
	e := newEnv(t, `"repos": {
		"app": {"url": "{{origin}}", "job_env": {"GOCACHE": "{{job}}/cache/go-build", "GOLANGCI_LINT_CACHE": "{{job}}/cache/golangci"},
			"job_seed": [{"from": "`+filepath.Join(shared, "go-build")+`", "to": "{{job}}/cache/go-build"}, {"from": "/no/such/dir", "to": "{{job}}/cache/gone"}]},
		"plain": "{{origin}}"
	}`)
	errOut := captureStderr(t)

	if code := e.run(t, "run", "--repo", "app", "--branch", "b", "--role", "review"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	job := filepath.Join(e.root, "work", "app", "b")
	want := "[env]\nGOCACHE = \"" + job + "/cache/go-build\"\nGOLANGCI_LINT_CACHE = \"" + job + "/cache/golangci\"\n"
	if got := readFile(t, filepath.Join(job, jobenv.File)); !strings.HasSuffix(got, want) {
		t.Errorf("mise.toml =\n%s\nwant it to end with\n%s", got, want)
	}
	if got := readFile(t, filepath.Join(job, "cache", "go-build", "00", "entry")); got != "warm" {
		t.Errorf("seeded cache entry = %q, want warm", got)
	}
	if !strings.Contains(errOut.String(), "warning: job_seed: not copying /no/such/dir") {
		t.Errorf("no warning for the missing seed:\n%s", errOut)
	}
	args := e.calls(t)[0][1]
	for _, d := range []string{"cache/gone", "cache/go-build", "cache/golangci"} {
		dir := filepath.Join(job, d)
		if !strings.Contains(args, "--add-dir "+dir+" ") {
			t.Errorf("claude not given --add-dir %s: %s", dir, args)
		}
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			t.Errorf("%s not made: %v", dir, err)
		}
	}

	// Resuming keeps them writable.
	removeFile(t, e.log)
	if code := e.run(t, "resume", "app/b", "--from", "1"); code != 0 {
		t.Fatalf("resume: exit %d", code)
	}
	if args := e.calls(t)[0][1]; !strings.Contains(args, "--add-dir "+filepath.Join(job, "cache", "golangci")+" ") {
		t.Errorf("resume lost the job's write dirs: %s", args)
	}

	// Without job_env and job_seed, nothing changes.
	removeFile(t, e.log)
	if code := e.run(t, "run", "--repo", "plain", "--branch", "b", "--role", "review"); code != 0 {
		t.Fatalf("plain: exit %d", code)
	}
	plain := filepath.Join(e.root, "work", "plain", "b")
	if _, err := os.Stat(filepath.Join(plain, jobenv.File)); err == nil {
		t.Error("wrote a mise.toml with no job_env")
	}
	if args := e.calls(t)[0][1]; strings.Count(args, "--add-dir ") != 2 { // tmp and context
		t.Errorf("plain run got extra --add-dir: %s", args)
	}

	// apply in your own checkout writes no mise.toml and adds no write dirs,
	// even for a repo with job_env.
	checkout := filepath.Join(e.root, "checkout")
	mustGit(t, "", "clone", "-q", e.origin, checkout)
	removeFile(t, e.log)
	if code := e.run(t, "apply", "--repo", "app", "--dir", checkout, "--prompt", "x"); code != 0 {
		t.Fatalf("apply: exit %d", code)
	}
	if _, err := os.Stat(filepath.Join(e.root, jobenv.File)); err == nil {
		t.Error("apply wrote a mise.toml beside your checkout")
	}
	if args := e.calls(t)[0][1]; strings.Contains(args, "cache") {
		t.Errorf("apply in your checkout got the job's write dirs: %s", args)
	}
}
