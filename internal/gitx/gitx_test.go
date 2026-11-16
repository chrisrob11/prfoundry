package gitx

import "testing"

func TestRunOKAndOutput(t *testing.T) {
	dir := t.TempDir()
	if OK(dir, "rev-parse", "--git-dir") {
		t.Fatal("OK in an empty folder: want false")
	}
	if err := Run(dir, "init", "--quiet", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if !OK(dir, "rev-parse", "--git-dir") {
		t.Fatal("OK after init: want true")
	}
	if got := Output(dir, "rev-parse", "--git-dir"); got != ".git" {
		t.Fatalf("Output = %q, want .git", got)
	}
	if got := Output(dir, "rev-parse", "HEAD"); got != "" {
		t.Fatalf("Output of a failing command = %q, want empty", got)
	}
	if err := Run(dir, "no-such-command"); err == nil {
		t.Fatal("Run of a bad command: want an error")
	}
}
