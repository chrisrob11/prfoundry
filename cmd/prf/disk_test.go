package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// fakeStatfs makes every existing path a filesystem with these block
// counts, and records the paths asked about.
func fakeStatfs(t *testing.T, blocks, bfree, bavail uint64) *[]string {
	t.Helper()
	var asked []string
	prev := statfs
	t.Cleanup(func() { statfs = prev })
	statfs = func(path string, st *syscall.Statfs_t) error {
		asked = append(asked, path)
		if _, err := os.Stat(path); err != nil {
			return syscall.ENOENT
		}
		st.Blocks, st.Bfree, st.Bavail = blocks, bfree, bavail
		return nil
	}
	return &asked
}

func TestDiskCheck(t *testing.T) {
	for _, tt := range []struct {
		name    string
		limit   int
		want    int
		wantErr string
	}{
		{"over the limit", 90, exitSetup, "is 95% full, at or above max_disk_percent (90%) in conf.json"},
		{"under the limit", 95, 0, ""}, // 90 of 95 usable blocks is 94.7%
		{"off", 0, 0, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, `"max_disk_percent": `+strconv.Itoa(tt.limit))
			captureStderr(t)
			asked := fakeStatfs(t, 100, 10, 5)
			err := newApp().Run(context.Background(), []string{"prf", "--home", e.home, "run", "--repo", e.origin, "--branch", "b", "--prompt", "x", "--role", "review"})
			if code := exitCode(t, err); code != tt.want {
				t.Fatalf("exit %d, want %d: %v", code, tt.want, err)
			}
			workdir := filepath.Join(e.root, "work")
			if tt.want != 0 {
				if msg := err.Error(); !strings.Contains(msg, "the disk holding "+workdir+" ") || !strings.Contains(msg, tt.wantErr) {
					t.Errorf("message = %q, want the workdir and %q", msg, tt.wantErr)
				}
				if _, err := os.Stat(filepath.Join(workdir, "origin", "b")); err == nil {
					t.Error("refused, but made the job folder anyway")
				}
				if n := len(e.calls(t)); n != 0 {
					t.Errorf("ran %d steps on a full disk", n)
				}
			} else if n := len(e.calls(t)); n != 1 {
				t.Errorf("ran %d steps, want 1", n)
			}
			switch {
			case tt.limit == 0 && len(*asked) != 0:
				t.Errorf("checked the disk with the check off: %v", *asked)
			case tt.limit != 0 && (len(*asked) != 2 || (*asked)[0] != workdir || (*asked)[1] != e.root):
				t.Errorf("should try the workdir, then its nearest parent that exists: %v", *asked)
			}
		})
	}
}

func TestDiskUsed(t *testing.T) {
	dir := t.TempDir()
	if used, err := diskUsed(dir); err != nil || used < 0 || used > 100 {
		t.Errorf("diskUsed(%s) on the real filesystem = %v, %v", dir, used, err)
	}

	fakeStatfs(t, 0, 0, 0)
	if used, err := diskUsed(dir); err != nil || used != 0 {
		t.Errorf("an empty filesystem = %v, %v; want 0", used, err)
	}

	statfs = func(string, *syscall.Statfs_t) error { return syscall.EACCES }
	if _, err := diskUsed(dir); !errors.Is(err, syscall.EACCES) {
		t.Errorf("err = %v, want EACCES", err)
	}
	if err := checkDisk(dir, 90); err == nil || !strings.Contains(err.Error(), "checking free space for "+dir) {
		t.Errorf("checkDisk = %v", err)
	}

	statfs = func(string, *syscall.Statfs_t) error { return syscall.ENOENT }
	if _, err := diskUsed(dir); !errors.Is(err, syscall.ENOENT) {
		t.Errorf("with no folder that exists: err = %v, want ENOENT", err)
	}
}
