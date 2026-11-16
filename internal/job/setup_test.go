package job

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

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
			if err := cloneRepo(origin, "", tt.branch, dir, "", t.Logf); err != nil {
				t.Fatal(err)
			}
			if got, err := gitOut(dir, "branch", "--show-current"); err != nil || got != tt.branch {
				t.Errorf("branch = %q, %v; want %q", got, err, tt.branch)
			}
			if got, err := gitOut(dir, "log", "-1", "--format=%s"); err != nil || got != tt.wantSubject {
				t.Errorf("HEAD subject = %q, want %q", got, tt.wantSubject)
			}
		})
	}

	t.Run("destination exists", func(t *testing.T) {
		err := cloneRepo(origin, "", "x", t.TempDir(), "", t.Logf)
		if err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("err = %v, want destination exists", err)
		}
	})

	t.Run("base branch", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "clone")
		if err := cloneRepo(origin, "existing", "feat/on-existing", dir, "", t.Logf); err != nil {
			t.Fatal(err)
		}
		if got, err := gitOut(dir, "log", "-1", "--format=%s"); err != nil || got != "on existing" {
			t.Errorf("HEAD subject = %q, %v; want branch cut from existing", got, err)
		}
	})
}

func TestCloneRepoWithReference(t *testing.T) {
	origin := newOrigin(t)
	checkout := filepath.Join(t.TempDir(), "checkout")
	runGit(t, "", "clone", "-q", origin, checkout)

	for name, reference := range map[string]string{"local checkout": checkout, "not a repo": t.TempDir(), "missing": "/nonexistent"} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "clone")
			if err := cloneRepo(origin, "", "feat/x", dir, reference, t.Logf); err != nil {
				t.Fatal(err)
			}
			if got, err := gitOut(dir, "log", "-1", "--format=%s"); err != nil || got != "init" {
				t.Errorf("HEAD subject = %q, %v", got, err)
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
	if err := switchBranch(dir, "existing", t.Logf); err != nil {
		t.Fatal(err)
	}
	if got, err := gitOut(dir, "log", "-1", "--format=%s"); err != nil || got != "on existing" {
		t.Errorf("switched to a new branch instead of the local one: HEAD = %q, %v", got, err)
	}
}

func TestDiskUsed(t *testing.T) {
	dir := t.TempDir()
	if used, err := diskUsed(dir, syscall.Statfs); err != nil || used < 0 || used > 100 {
		t.Errorf("diskUsed(%s) on the real filesystem = %v, %v", dir, used, err)
	}

	empty := func(path string, st *syscall.Statfs_t) error {
		if _, err := os.Stat(path); err != nil {
			return syscall.ENOENT
		}
		st.Blocks, st.Bfree, st.Bavail = 0, 0, 0
		return nil
	}
	if used, err := diskUsed(dir, empty); err != nil || used != 0 {
		t.Errorf("an empty filesystem = %v, %v; want 0", used, err)
	}

	denied := func(string, *syscall.Statfs_t) error { return syscall.EACCES }
	if _, err := diskUsed(dir, denied); !errors.Is(err, syscall.EACCES) {
		t.Errorf("err = %v, want EACCES", err)
	}
	if err := checkDisk(dir, 90, denied); err == nil || !strings.Contains(err.Error(), "checking free space for "+dir) {
		t.Errorf("checkDisk = %v", err)
	}

	missing := func(string, *syscall.Statfs_t) error { return syscall.ENOENT }
	if _, err := diskUsed(dir, missing); !errors.Is(err, syscall.ENOENT) {
		t.Errorf("with no folder that exists: err = %v, want ENOENT", err)
	}
}
