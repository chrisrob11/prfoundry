package main

import (
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
	writeFileT(t, filepath.Join(dir, "README.md"), "# origin\n")
	mustGit(t, dir, "add", "README.md")
	mustGit(t, dir, "commit", "-q", "-m", "init")
	mustGit(t, dir, "switch", "-q", "-c", "existing")
	mustGit(t, dir, "commit", "-q", "--allow-empty", "-m", "on existing")
	mustGit(t, dir, "switch", "-q", "main")
	return dir
}
