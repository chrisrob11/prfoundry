package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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

func TestCloneRepo(t *testing.T) {
	origin := newOrigin(t)

	tests := []struct {
		name, branch, wantSubject string
	}{
		{name: "new branch from default", branch: "feat/new", wantSubject: "init"},
		{name: "existing remote branch", branch: "existing", wantSubject: "on existing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "clone")
			if err := cloneRepo(origin, "", tt.branch, dir, ""); err != nil {
				t.Fatal(err)
			}
			if got := mustGit(t, dir, "branch", "--show-current"); got != tt.branch {
				t.Errorf("branch = %q, want %q", got, tt.branch)
			}
			if got := mustGit(t, dir, "log", "-1", "--format=%s"); got != tt.wantSubject {
				t.Errorf("HEAD subject = %q, want %q", got, tt.wantSubject)
			}
		})
	}

	t.Run("destination exists", func(t *testing.T) {
		err := cloneRepo(origin, "", "x", t.TempDir(), "")
		if err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("err = %v, want destination exists", err)
		}
	})

	t.Run("base branch", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "clone")
		if err := cloneRepo(origin, "existing", "feat/on-existing", dir, ""); err != nil {
			t.Fatal(err)
		}
		if got := mustGit(t, dir, "log", "-1", "--format=%s"); got != "on existing" {
			t.Errorf("HEAD subject = %q, want branch cut from existing", got)
		}
	})
}

func TestCloneRepoWithReference(t *testing.T) {
	origin := newOrigin(t)
	checkout := filepath.Join(t.TempDir(), "checkout")
	mustGit(t, "", "clone", "-q", origin, checkout)

	for name, reference := range map[string]string{"local checkout": checkout, "not a repo": t.TempDir(), "missing": "/nonexistent"} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "clone")
			if err := cloneRepo(origin, "", "feat/x", dir, reference); err != nil {
				t.Fatal(err)
			}
			if got := mustGit(t, dir, "log", "-1", "--format=%s"); got != "init" {
				t.Errorf("HEAD subject = %q", got)
			}
			// --dissociate: the clone keeps no link to the reference's objects.
			if _, err := os.Stat(filepath.Join(dir, ".git", "objects", "info", "alternates")); err == nil {
				t.Error("clone still borrows objects from the reference")
			}
		})
	}
}

func TestSwitchBranchLocal(t *testing.T) {
	dir := newOrigin(t) // has a local branch "existing" and no origin remote
	if err := switchBranch(dir, "existing"); err != nil {
		t.Fatal(err)
	}
	if got := mustGit(t, dir, "log", "-1", "--format=%s"); got != "on existing" {
		t.Errorf("switched to a new branch instead of the local one: HEAD = %q", got)
	}
}

func TestOpenInput(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "ctx.txt")
	writeFileT(t, file, "context")
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("regular file goes to every phase", func(t *testing.T) {
		for _, first := range []bool{true, false} {
			r, err := openInput(file, first)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(r)
			r.(io.Closer).Close()
			if string(b) != "context" {
				t.Errorf("first=%v: read %q", first, b)
			}
		}
	})

	tests := []struct {
		name   string
		in     string
		first  bool
		wantIn io.Reader
	}{
		{name: "none", in: "", first: true, wantIn: nil},
		{name: "stdin first phase", in: "-", first: true, wantIn: os.Stdin},
		{name: "stdin later phase", in: "-", first: false, wantIn: nil},
		{name: "fifo later phase", in: fifo, first: false, wantIn: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := openInput(tt.in, tt.first)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.wantIn {
				t.Errorf("got %v, want %v", got, tt.wantIn)
			}
		})
	}

	if _, err := openInput(filepath.Join(dir, "missing"), true); err == nil {
		t.Error("missing file: want error")
	}
}
