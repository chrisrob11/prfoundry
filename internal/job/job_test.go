package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	claudeagent "github.com/chrisrob11/prfoundry/internal/claude"
	"github.com/chrisrob11/prfoundry/internal/pipeline"
	"github.com/chrisrob11/prfoundry/internal/state"
	"github.com/chrisrob11/prfoundry/internal/textfile"
)

// fakeClaude records each call as "cwd\nargs\n---\n" in $FAKE_LOG, and
// reports a result costing $0.05. By prompt: ASK writes a question to
// $FAKE_ASK_PATH ($PRF_TMP/stop.md if unset) while $FAKE_ASKS counts down
// above zero, and ANSWER deletes it; LIMIT hits the usage limit likewise
// with $FAKE_LIMITS; COMMIT commits to fixed.txt; DIRTY leaves seven
// untracked files; FAIL, or one holding $FAKE_FAIL_ON if set, exits 1.
const fakeClaude = `#!/bin/sh
printf '%s\n%s\n---\n' "$(pwd)" "$*" >> "$FAKE_LOG"
FAKE_ASK_PATH=${FAKE_ASK_PATH:-$PRF_TMP/stop.md}
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
if [ -n "$FAKE_FAIL_ON" ]; then case "$2" in *"$FAKE_FAIL_ON"*) FAIL=1;; esac; fi
case "$2" in *FAIL*) FAIL=1;; esac
if [ -n "$FAIL" ]; then
  printf '%s\n' '{"type":"result","is_error":true,"result":"it broke","num_turns":2,"total_cost_usd":0.01}'; exit 1
fi
printf '%s\n' '{"type":"result","is_error":false,"result":"did the thing","num_turns":3,"total_cost_usd":0.05}'
`

// testJob runs a job end to end, as prf run and prf resume do: an origin
// repo, a prf home holding the shared roles, and the fake claude. A test
// changes o and clone before it runs.
type testJob struct {
	o      Options
	clone  Clone
	root   string
	roles  string // the shared roles folder
	log    string // the fake claude's calls
	stderr string // the job's progress and warnings
}

// newTestJob is a job of steps on branch b of repo origin, in a temp dir
// that becomes the working directory, with roles implement, test and review.
func newTestJob(t *testing.T, steps ...pipeline.Step) *testJob {
	t.Helper()
	tj := &testJob{clone: Clone{URL: newOrigin(t), Branch: "b"}, root: t.TempDir()}
	t.Chdir(tj.root)
	home := filepath.Join(tj.root, "home")
	tj.roles = filepath.Join(home, "roles")
	tj.log = filepath.Join(tj.root, "claude.log")
	tj.stderr = filepath.Join(tj.root, "stderr")
	t.Setenv("FAKE_LOG", tj.log)
	claude := filepath.Join(tj.root, "claude")
	writeFile(t, claude, fakeClaude)
	if err := os.Chmod(claude, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"implement", "test", "review"} {
		writeRole(t, tj.roles, name, name+" on {{.Branch}}")
	}
	stderr, err := os.Create(tj.stderr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stderr.Close() })
	tj.o = Options{
		Home: home, Repo: "origin", Steps: steps, Pipeline: "default", Workdir: filepath.Join(tj.root, "work"),
		Mode: "acceptEdits", Agent: claudeagent.New(claude), Sandbox: true, ReviewOpen: true,
		Stdout: io.Discard, Stderr: stderr,
	}
	return tj
}

// parseSteps reads steps as pipeline.json spells them, in a JSON array.
func parseSteps(t *testing.T, list string) []pipeline.Step {
	t.Helper()
	var steps []pipeline.Step
	if err := json.Unmarshal([]byte(list), &steps); err != nil {
		t.Fatal(err)
	}
	return steps
}

// jobDir is where the job's status, state and checkout are.
func (tj *testJob) jobDir() string {
	return filepath.Join(tj.o.Workdir, "origin", "b")
}

// runErr runs the job as prf run does, returning its outcome.
func (tj *testJob) runErr(t *testing.T) error {
	t.Helper()
	j, err := New(tj.o)
	if err != nil {
		t.Fatal(err)
	}
	return j.Run(context.Background(), tj.clone)
}

// run runs the job as prf run does, returning its exit code.
func (tj *testJob) run(t *testing.T) int {
	t.Helper()
	return exitOf(t, tj.runErr(t))
}

// resume carries on the stopped job from the step it stopped at, as prf
// resume does, returning its exit code.
func (tj *testJob) resume(t *testing.T) int {
	t.Helper()
	st := readState(t, tj.jobDir())
	return exitOf(t, Resume(context.Background(), tj.o, st, st.Step))
}

// calls returns each recorded claude call as {cwd, args}.
func (tj *testJob) calls(t *testing.T) [][2]string {
	t.Helper()
	b, err := os.ReadFile(tj.log)
	if errors.Is(err, fs.ErrNotExist) {
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

// logs is what the job has written to stderr so far.
func (tj *testJob) logs(t *testing.T) string {
	t.Helper()
	return readFile(t, tj.stderr)
}

// exitOf is the exit code a job's outcome gives prf.
func exitOf(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var ee exitError
	if !errors.As(err, &ee) {
		t.Fatalf("not an exit: %v", err)
	}
	return ee.code
}

func readState(t *testing.T, jobDir string) state.Job {
	t.Helper()
	st, err := state.Read(jobDir)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func writeFile(t *testing.T, path, s string) {
	t.Helper()
	if err := textfile.Write(path, s); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, _ := os.ReadFile(path)
	return string(b)
}

// removeFile removes path, which may not exist, failing t on any other error.
func removeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
}

func writeRole(t *testing.T, dir, name, text string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, name+".md"), text)
}

// roleSteps makes steps with no model.
func roleSteps(names ...string) []pipeline.Step {
	steps := make([]pipeline.Step, len(names))
	for i, n := range names {
		steps[i] = pipeline.Step{Role: n}
	}
	return steps
}

// isolateGit keeps tests away from the user's git config, gives commits an
// identity, and stops git's background gc and maintenance, which can still be
// writing into .git when a test's temp dir is removed.
func isolateGit(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for k, v := range map[string]string{
		"GIT_CONFIG_COUNT": "2",
		"GIT_CONFIG_KEY_0": "gc.auto", "GIT_CONFIG_VALUE_0": "0",
		"GIT_CONFIG_KEY_1": "maintenance.auto", "GIT_CONFIG_VALUE_1": "false",
	} {
		t.Setenv(k, v)
	}
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "prf test")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "test@example.com")
	}
}

// runGit runs git in dir to set a test up, failing the test if it fails.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if _, err := gitOut(dir, args...); err != nil {
		t.Fatal(err)
	}
}

// gitOut is git's trimmed output in dir.
func gitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %v: %w\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// newOrigin creates a repo on main with a README in one commit and a branch
// "existing" that has a second commit.
func newOrigin(t *testing.T) string {
	t.Helper()
	isolateGit(t)
	dir := filepath.Join(t.TempDir(), "origin")
	runGit(t, "", "init", "-q", "-b", "main", dir)
	writeFile(t, filepath.Join(dir, "README.md"), "# origin\n")
	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "commit", "-q", "-m", "init")
	runGit(t, dir, "switch", "-q", "-c", "existing")
	runGit(t, dir, "commit", "-q", "--allow-empty", "-m", "on existing")
	runGit(t, dir, "switch", "-q", "main")
	return dir
}
