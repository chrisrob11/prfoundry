package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

// sameDir compares paths after resolving symlinks (macOS temp dirs live under /private).
func sameDir(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}

func TestSumKeepsRecentJobsAndNamesUnrecordedCost(t *testing.T) {
	now := time.Now()
	states := []Job{
		{Cost: 2, Updated: now, Spend: []Spend{{Role: "a", Model: "sonnet", Runs: 1, Cost: 0.5}, {Role: "b", Model: "opus", Runs: 1, Cost: 1.5}}},
		{Cost: 3, Updated: now.Add(-time.Hour)}, // from before spend was recorded
		{Cost: 9, Updated: now.Add(-72 * time.Hour), Spend: []Spend{{Role: "a", Model: "sonnet", Cost: 9}}}, // too old
	}
	rows, total, unrecorded := Sum(states, 48*time.Hour, now)
	if total != 5 || unrecorded != 3 || len(rows) != 2 || rows[0].Role != "b" || rows[1].Cost != 0.5 {
		t.Errorf("Sum = %+v total %v unrecorded %v", rows, total, unrecorded)
	}
	if rows, total, _ := Sum(states, 0, now); total != 14 || rows[0].Role != "a" || rows[0].Cost != 9.5 {
		t.Errorf("no --since = %+v total %v, want every job", rows, total)
	}
}

// TestFindFromTheWorkdir: a job named from inside the workdir, where
// <repo>/<job> is also a relative path, comes back with an absolute folder.
func TestFindFromTheWorkdir(t *testing.T) {
	workdir := t.TempDir()
	jobDir := filepath.Join(workdir, "origin", "b")
	writeFile(t, filepath.Join(jobDir, FileName), `{"repo":"origin"}`)
	t.Chdir(workdir)
	for _, target := range []string{"origin/b", jobDir} {
		s, err := Find("elsewhere", target)
		if err != nil {
			t.Fatalf("Find(%q): %v", target, err)
		}
		if !sameDir(s.JobDir, jobDir) || !filepath.IsAbs(s.JobDir) {
			t.Errorf("Find(%q).JobDir = %q, want %q", target, s.JobDir, jobDir)
		}
	}
}
