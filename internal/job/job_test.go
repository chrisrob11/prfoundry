package job

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chrisrob11/prfoundry/internal/pipeline"
	"github.com/chrisrob11/prfoundry/internal/textfile"
)

// fakeClaude records each call as "cwd\nargs\n---\n" in $FAKE_LOG, and
// reports a result costing $0.05.
const fakeClaude = `#!/bin/sh
printf '%s\n%s\n---\n' "$(pwd)" "$*" >> "$FAKE_LOG"
printf '%s\n' '{"type":"result","is_error":false,"result":"did the thing","num_turns":3,"total_cost_usd":0.05}'
`

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

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newOrigin creates a repo on main with a README in one commit and a branch
// "existing" that has a second commit.
func newOrigin(t *testing.T) string {
	t.Helper()
	isolateGit(t)
	dir := filepath.Join(t.TempDir(), "origin")
	mustGit(t, "", "init", "-q", "-b", "main", dir)
	writeFile(t, filepath.Join(dir, "README.md"), "# origin\n")
	mustGit(t, dir, "add", "README.md")
	mustGit(t, dir, "commit", "-q", "-m", "init")
	mustGit(t, dir, "switch", "-q", "-c", "existing")
	mustGit(t, dir, "commit", "-q", "--allow-empty", "-m", "on existing")
	mustGit(t, dir, "switch", "-q", "main")
	return dir
}
