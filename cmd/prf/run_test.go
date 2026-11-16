package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

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
	writeFileT(t, filepath.Join(dir, "README.md"), "# origin\n")
	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "commit", "-q", "-m", "init")
	runGit(t, dir, "switch", "-q", "-c", "existing")
	runGit(t, dir, "commit", "-q", "--allow-empty", "-m", "on existing")
	runGit(t, dir, "switch", "-q", "main")
	return dir
}
