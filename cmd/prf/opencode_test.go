package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chrisrob11/prfoundry/internal/agent"
	"github.com/chrisrob11/prfoundry/internal/job"
	"github.com/chrisrob11/prfoundry/internal/pipeline"
	"github.com/chrisrob11/prfoundry/internal/state"
)

// fakeOpenCode records each call in $FAKE_LOG as fakeClaude does, and its
// permission config in <log>.config. It refuses a call that isn't
// `opencode run --standalone --format json`. (It can't check PWD, which
// opencode works in: sh resets it.) It prints the events of a real run
// (internal/opencode/testdata/run.jsonl), then its last step's finish,
// costing $0.05. Its prompt is its last argument: one containing FAIL
// prints an error and exits 1; COMMIT commits a change to fixed.txt; LIMIT
// hits a rate limit while the count in $FAKE_LIMITS is above zero; BUDGET
// spends $0.30 and waits to be interrupted.
const fakeOpenCode = `#!/bin/sh
for prompt; do :; done
printf '%s\n%s\n---\n' "$(pwd)" "$*" >> "$FAKE_LOG"
printf '%s\n' "$OPENCODE_CONFIG_CONTENT" >> "$FAKE_LOG.config"
[ "$1 $2 $3 $4" = "run --standalone --format json" ] || { echo "fake opencode: bad args: $*" >&2; exit 64; }
case "$prompt" in *LIMIT*)
  n=$(cat "$FAKE_LIMITS" 2>/dev/null || echo 0)
  if [ "$n" -gt 0 ]; then
    echo $((n - 1)) > "$FAKE_LIMITS"
    printf '%s\n' '{"type":"error","error":{"type":"provider.rate-limit","message":"Rate limited","status":429,"retryAfterMs":3600000}}'; exit 1
  fi;;
esac
case "$prompt" in *FAIL*) printf '%s\n' '{"type":"error","error":{"type":"provider.error","message":"it broke"}}'; exit 1;; esac
case "$prompt" in *COMMIT*) echo x >> fixed.txt && git add fixed.txt && git commit -qm "fix: fake";; esac
case "$prompt" in *BUDGET*)
  trap 'exit 130' INT
  printf '%s\n' '{"type":"step_start"}' '{"type":"step_finish","part":{"cost":0.30}}'
  sleep 10 >/dev/null & wait; exit 0;;
esac
cat "$FAKE_OPENCODE_RUN"
printf '%s\n' '{"type":"step_finish","part":{"reason":"stop","cost":0.05,"tokens":{"cache":{"read":1000}}}}'
`

// openCodeEnv is newEnv with conf (JSON object members) plus opencode as
// the agent, unsandboxed, and $OPENCODE_BIN the fake.
func openCodeEnv(t *testing.T, conf string) env {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "opencode")
	writeFileT(t, bin, fakeOpenCode)
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	fixture, err := filepath.Abs("../../internal/opencode/testdata/run.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_OPENCODE_RUN", fixture)
	t.Setenv("OPENCODE_BIN", bin)
	members := `"agent": "opencode", "sandbox": false`
	if conf != "" {
		members += ", " + conf
	}
	e := newEnv(t, members)
	e.captureStderr()
	return e
}

func TestOpenCodeRun(t *testing.T) {
	e := openCodeEnv(t, `"opencode_args": ["--agent", "build"]`)
	writeFileT(t, filepath.Join(e.home, "origin", pipeline.FileName),
		`{"default": "a", "opencode_model": "anthropic/claude-sonnet-5-5", "pipelines": {"a": ["implement", {"role": "test", "opencode_model": "ollama/gemma4:31b", "model": "opus"}, "review"]}}`)

	if code := e.run(t, "run", "--repo", e.origin, "--branch", "b", "--prompt", "x"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	calls := e.calls(t)
	if len(calls) != 3 {
		t.Fatalf("got %d opencode calls, want 3: %v", len(calls), calls)
	}
	jobDir := filepath.Join(e.root, "work", "origin", "b")
	for i, model := range []string{"anthropic/claude-sonnet-5-5", "ollama/gemma4:31b", "anthropic/claude-sonnet-5-5"} {
		cwd, args := calls[i][0], calls[i][1]
		if !sameDir(cwd, filepath.Join(jobDir, "repo")) {
			t.Errorf("call %d ran in %s", i, cwd)
		}
		if want := "run --standalone --format json --model " + model + " --agent build "; !strings.HasPrefix(args, want) {
			t.Errorf("call %d: args %q, want them to start %q", i, args, want)
		}
	}
	if conf := readFile(t, e.log+".config"); !strings.Contains(conf, `{"action":"edit","resource":"*","effect":"allow"}`) || !strings.Contains(conf, `{"action":"shell","resource":"gh *","effect":"deny"}`) {
		t.Errorf("permission config for acceptEdits = %s", conf)
	}
	status := readFile(t, filepath.Join(jobDir, "status"))
	if want := "PRF_PHASE branch=b phase=test step=2/3 exit=0 error=false turns=2 cost=0.05\n"; !strings.Contains(status, want) {
		t.Errorf("status missing %q:\n%s", want, status)
	}
	if log := readFile(t, filepath.Join(jobDir, "claude.log")); !strings.Contains(log, "  [read] a.txt\n") || !strings.Contains(log, "--- review summary ---\nok\n") {
		t.Errorf("log:\n%s", log)
	}
	st := readState(t, jobDir)
	if st.Agent != agent.OpenCode || st.State != state.Done || st.Cost < 0.149 || st.Models[1] != "ollama/gemma4:31b" {
		t.Errorf("state = %+v", st)
	}
}

func TestOpenCodeCheckGateFixRound(t *testing.T) {
	e := openCodeEnv(t, "")
	writeRole(t, e.roles, pipeline.Step{Gate: pipeline.GateCheck}.Fixer(), "COMMIT fix-check on {{.Branch}}\n{{.Review}}")
	writeFileT(t, filepath.Join(e.home, "origin", pipeline.FileName),
		`{"default": "a", "pipelines": {"a": ["implement", {"gate": "check", "run": "test -f fixed.txt || { echo '--- BROKE: TestX'; exit 1; }"}, "review"]}}`)

	if code := e.run(t, "run", "--repo", e.origin, "--branch", "b", "--prompt", "x"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	calls := e.calls(t)
	if len(calls) != 3 || !strings.Contains(calls[1][1], "COMMIT fix-check on b") || !strings.Contains(calls[1][1], "--- BROKE: TestX") {
		t.Fatalf("want implement, a fix-check given the failure, then review: %v", calls)
	}
	status := readFile(t, filepath.Join(e.root, "work", "origin", "b", "status"))
	for _, want := range []string{"round=1 exit=1\n", "PRF_PHASE branch=b phase=fix-check step=2/3 exit=0", "round=2 exit=0\n", "phase=@check step=2/3 exit=0 rounds=2\n"} {
		if !strings.Contains(status, want) {
			t.Errorf("status missing %q:\n%s", want, status)
		}
	}
}

func TestOpenCodeResumeKeepsTheAgent(t *testing.T) {
	e := openCodeEnv(t, "")
	writeRole(t, e.roles, "test", "FAIL")
	if code := e.run(t, "run", "--repo", e.origin, "--branch", "b", "--prompt", "x", "--model", "anthropic/claude-haiku-5-5"); code != 1 {
		t.Fatalf("run: exit %d, want 1", code)
	}
	jobDir := filepath.Join(e.root, "work", "origin", "b")
	if st := readState(t, jobDir); st.Agent != agent.OpenCode {
		t.Fatalf("state.json's agent = %q", st.Agent)
	}

	// conf.json no longer picks opencode, but the job keeps it, and its model.
	writeFileT(t, filepath.Join(e.home, "conf.json"), `{"claude": "`+filepath.Join(e.root, "claude")+`", "workdir": "`+filepath.Join(e.root, "work")+`", "sandbox": false}`)
	writeRole(t, e.roles, "test", "test on {{.Branch}}")
	removeFile(t, e.log)
	if code := e.run(t, "resume", "origin/b"); code != 0 {
		t.Fatalf("resume: exit %d", code)
	}
	calls := e.calls(t)
	if len(calls) != 2 {
		t.Fatalf("resume: %d calls, want test and review: %v", len(calls), calls)
	}
	for _, c := range calls {
		if !strings.HasPrefix(c[1], "run --standalone --format json --model anthropic/claude-haiku-5-5 ") {
			t.Errorf("resume didn't run opencode on the job's model: %s", c[1])
		}
	}

	// --agent switches it, and the models are then the steps' own.
	removeFile(t, e.log)
	if code := e.run(t, "resume", "origin/b", "--from", "3", "--agent", "claude"); code != 0 {
		t.Fatalf("resume --agent claude: exit %d", code)
	}
	if calls := e.calls(t); len(calls) != 1 || !strings.HasPrefix(calls[0][1], "-p ") || strings.Contains(calls[0][1], "--model") {
		t.Errorf("resume --agent claude ran %v", calls)
	}
	if st := readState(t, jobDir); st.Agent != agent.Claude {
		t.Errorf("state.json's agent = %q after switching", st.Agent)
	}
}

func TestOpenCodeBudgetStopsTheRun(t *testing.T) {
	e := openCodeEnv(t, "")
	writeRole(t, e.roles, "implement", "BUDGET")
	start := time.Now()
	if code := e.run(t, "run", "--repo", e.origin, "--branch", "b", "--prompt", "x", "--max-step-cost", "0.25"); code != job.ExitBudget {
		t.Fatalf("exit %d, want %d", code, job.ExitBudget)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("took %v: the run wasn't stopped at its budget", took)
	}
	status := readFile(t, filepath.Join(e.root, "work", "origin", "b", "status"))
	if !strings.Contains(status, "phase=implement step=1/3 exit=5 error=false turns=1 cost=0.30 stopped=max_cost") {
		t.Errorf("status:\n%s", status)
	}
}

func TestOpenCodeLimitPausesTheJob(t *testing.T) {
	e := openCodeEnv(t, "")
	writeRole(t, e.roles, "test", "LIMIT")
	limits := filepath.Join(e.root, "limits")
	writeFileT(t, limits, "1\n")
	t.Setenv("FAKE_LIMITS", limits)
	var asked time.Time
	e.h.rt.PauseFor = func(reset time.Time) time.Duration { asked = reset; return 10 * time.Millisecond }
	if code := e.run(t, "run", "--repo", e.origin, "--branch", "b", "--prompt", "x"); code != 0 {
		t.Fatalf("exit %d, want 0: the step runs again after the pause", code)
	}
	if n := len(e.calls(t)); n != 4 {
		t.Errorf("got %d calls, want 4 (implement, test twice, review)", n)
	}
	if wait := time.Until(asked); wait < 59*time.Minute || wait > time.Hour {
		t.Errorf("paused until %v, want an hour from now, as retryAfterMs says", asked)
	}
}

func TestOpenCodeRefusals(t *testing.T) {
	e := openCodeEnv(t, "")
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--sandbox"}, "--sandbox=false"},
		{[]string{"--mode", "auto"}, `permission mode "auto"`},
		{[]string{"--model", "opus"}, "provider/model"},
		{[]string{"--role", "implement:opus"}, "opencode_model"},
		{[]string{"--agent", "codex"}, `--agent must be "claude" or "opencode"`},
	} {
		args := append([]string{"run", "--repo", e.origin, "--branch", "b", "--prompt", "x"}, c.args...)
		if code, msg := e.runErr(t, args...); code != job.ExitUsage || !strings.Contains(msg, c.want) {
			t.Errorf("%v: exit %d, %q; want %d and %q", c.args, code, msg, job.ExitUsage, c.want)
		}
	}
	if calls := e.calls(t); len(calls) != 0 {
		t.Errorf("a refused run called opencode: %v", calls)
	}
	if _, err := os.Stat(filepath.Join(e.root, "work", "origin", "b")); err == nil {
		t.Error("a refused run made its job folder")
	}
}
