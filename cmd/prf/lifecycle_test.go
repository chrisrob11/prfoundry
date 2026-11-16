package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chrisrob11/prfoundry/internal/job"
	"github.com/chrisrob11/prfoundry/internal/state"
)

// fakeGH records each call in $FAKE_GH_LOG. "pr list --state open" finds the
// PR in $FAKE_GH_EXISTING, if set, and "pr view" also finds a merged one in
// $FAKE_GH_MERGED; "pr create" prints a PR URL.
const fakeGH = `#!/bin/sh
echo "$*" >> "$FAKE_GH_LOG"
case "$1 $2" in
  "pr list") echo "$FAKE_GH_EXISTING";;
  "pr view") if [ -n "$FAKE_GH_EXISTING$FAKE_GH_MERGED" ]; then echo "$FAKE_GH_EXISTING$FAKE_GH_MERGED"; exit 0; fi; echo "no pull requests found" >&2; exit 1;;
  "pr create") echo "Creating pull request..."; echo "https://github.com/me/app/pull/7";;
esac
`

// withFakeGH puts the fake gh first on PATH and returns its call log.
func withFakeGH(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	writeFileT(t, filepath.Join(bin, "gh"), fakeGH)
	if err := os.Chmod(filepath.Join(bin, "gh"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	log := filepath.Join(bin, "calls")
	t.Setenv("FAKE_GH_LOG", log)
	t.Setenv("FAKE_GH_EXISTING", "")
	return log
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, _ := os.ReadFile(path)
	return string(b)
}

func TestPushAndPR(t *testing.T) {
	e := newEnv(t, `"repos": {
		"app": {"url": "{{origin}}", "push": true, "pr_create": "draft"},
		"titled": {"url": "{{origin}}", "pr_create": "ready", "pr_title_file": "README.md", "pr_body_file": "README.md"},
		"untitled": {"url": "{{origin}}", "pr_create": "ready", "pr_title_file": "TITLE.txt", "pr_body_file": "README.md"}
	}`)
	ghLog := withFakeGH(t)
	e.captureStderr()
	run := func(repo, branch string, extra ...string) int {
		removeFile(t, ghLog)
		return e.run(t, append([]string{"run", "--repo", repo, "--branch", branch, "--prompt", "x", "--role", "review"}, extra...)...)
	}
	onOrigin := func(branch string) bool { return mustGit(t, e.origin, "branch", "--list", branch) != "" }

	if code := run("app", "a"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	jobDir := filepath.Join(e.root, "work", "app", "a")
	if !onOrigin("a") {
		t.Error("branch a was not pushed")
	}
	status := readFile(t, filepath.Join(jobDir, "status"))
	for _, want := range []string{"PRF_PUSH branch=a exit=0\n", "PRF_PR branch=a url=https://github.com/me/app/pull/7\n"} {
		if !strings.Contains(status, want) {
			t.Errorf("status missing %q:\n%s", want, status)
		}
	}
	if strings.Index(status, "PRF_PR") > strings.Index(status, "PRF_DONE") {
		t.Error("PRF_PR should come before PRF_DONE")
	}
	if s := readState(t, jobDir); !s.Pushed || s.PRURL != "https://github.com/me/app/pull/7" || !s.Push || s.PRCreate != "draft" {
		t.Errorf("state = %+v", s)
	}
	if calls := readFile(t, ghLog); !strings.Contains(calls, "pr create --head a --draft --fill\n") {
		t.Errorf("gh calls:\n%s", calls)
	}

	t.Run("--push=false skips pushing and the PR", func(t *testing.T) {
		if code := run("app", "b", "--push=false"); code != 0 {
			t.Fatalf("exit %d", code)
		}
		if onOrigin("b") || readFile(t, ghLog) != "" {
			t.Error("pushed or called gh anyway")
		}
	})

	t.Run("an open PR is reused", func(t *testing.T) {
		t.Setenv("FAKE_GH_EXISTING", "https://github.com/me/app/pull/3")
		if code := run("app", "c"); code != 0 {
			t.Fatalf("exit %d", code)
		}
		if strings.Contains(readFile(t, ghLog), "pr create") {
			t.Error("created a second PR")
		}
		if s := readState(t, filepath.Join(e.root, "work", "app", "c")); s.PRURL != "https://github.com/me/app/pull/3" {
			t.Errorf("pr_url = %q", s.PRURL)
		}
	})

	t.Run("a merged PR on the same branch name gets a new one", func(t *testing.T) {
		t.Setenv("FAKE_GH_MERGED", "https://github.com/me/app/pull/2")
		if code := run("app", "c2"); code != 0 {
			t.Fatalf("exit %d", code)
		}
		if !strings.Contains(readFile(t, ghLog), "pr create") {
			t.Error("took the merged PR for an open one")
		}
		if s := readState(t, filepath.Join(e.root, "work", "app", "c2")); s.PRURL != "https://github.com/me/app/pull/7" {
			t.Errorf("pr_url = %q", s.PRURL)
		}
	})

	t.Run("title and body files from the checkout", func(t *testing.T) {
		if code := run("titled", "d"); code != 0 {
			t.Fatalf("exit %d", code)
		}
		readme := filepath.Join(e.root, "work", "titled", "d", "repo", "README.md")
		if calls := readFile(t, ghLog); !strings.Contains(calls, "pr create --head d --title # origin --body-file "+readme+"\n") {
			t.Errorf("gh calls:\n%s", calls)
		}
	})

	t.Run("a missing title file means pushed but no PR, exit 4", func(t *testing.T) {
		if code := run("untitled", "f"); code != job.ExitShip {
			t.Fatalf("exit %d, want %d", code, job.ExitShip)
		}
		if !onOrigin("f") {
			t.Error("should still have pushed")
		}
		if status := readFile(t, filepath.Join(e.root, "work", "untitled", "f", "status")); !strings.Contains(status, "PRF_PR branch=f exit=1\n") {
			t.Errorf("status:\n%s", status)
		}
	})

	t.Run("a failed push means exit 4 and no PR", func(t *testing.T) {
		checkout := filepath.Join(e.root, "nowhere")
		mustGit(t, "", "clone", "-q", e.origin, checkout)
		mustGit(t, checkout, "remote", "set-url", "origin", "/nonexistent/repo")
		removeFile(t, ghLog)
		if code := e.run(t, "apply", "--dir", checkout, "--branch", "g", "--prompt", "x", "--pr-create", "draft"); code != job.ExitShip {
			t.Fatalf("exit %d, want %d", code, job.ExitShip)
		}
		if readFile(t, ghLog) != "" {
			t.Error("opened a PR after a failed push")
		}
		if status := readFile(t, filepath.Join(e.root, "work", "nowhere", "g", "status")); !strings.Contains(status, "PRF_PUSH branch=g exit=") || strings.Contains(status, "PRF_PUSH branch=g exit=0") {
			t.Errorf("status:\n%s", status)
		}
	})

	t.Run("no push when a step fails", func(t *testing.T) {
		writeRole(t, e.roles, "review", "FAIL")
		t.Cleanup(func() { writeRole(t, e.roles, "review", "review on {{.Branch}}") })
		if code := run("app", "h"); code != 1 {
			t.Fatalf("exit %d", code)
		}
		if onOrigin("h") || readFile(t, ghLog) != "" {
			t.Error("shipped work whose step failed")
		}
	})
}

func TestResume(t *testing.T) {
	e := newEnv(t, "")
	e.captureStderr()
	writeRole(t, e.roles, "test", "FAIL")
	if code := e.run(t, "run", "--repo", e.origin, "--branch", "b", "--prompt", "the task"); code != 1 {
		t.Fatalf("run: exit %d", code)
	}
	jobDir := filepath.Join(e.root, "work", "origin", "b")
	before := readState(t, jobDir)

	writeRole(t, e.roles, "test", "test on {{.Branch}}")
	removeFile(t, e.log)
	if code := e.run(t, "resume", "origin/b"); code != 0 {
		t.Fatalf("resume: exit %d", code)
	}
	calls := e.calls(t)
	if len(calls) != 2 || !strings.HasPrefix(strings.TrimPrefix(calls[0][1], "-p "), "test on b") || !strings.Contains(calls[0][1], "## Task\n\nthe task") {
		t.Errorf("resume should run test then review, with the job's prompt: %v", calls)
	}
	if !sameDir(calls[0][0], filepath.Join(jobDir, "repo")) {
		t.Errorf("resume ran in %s", calls[0][0])
	}
	status := readFile(t, filepath.Join(jobDir, "status"))
	for _, want := range []string{"phase=test step=2/3 exit=1", "phase=test step=2/3 pipeline=default\n", "phase=review step=3/3 exit=0", "PRF_DONE branch=b phase=review exit=0"} {
		if !strings.Contains(status, want) {
			t.Errorf("status missing %q:\n%s", want, status)
		}
	}
	after := readState(t, jobDir)
	if after.State != state.Done || after.Step != 3 || len(after.Roles) != 3 || !after.Started.Equal(before.Started) || after.Cost < before.Cost+0.099 {
		t.Errorf("state after resume = %+v (before: %+v)", after, before)
	}

	if code := e.run(t, "resume", "origin/b"); code != job.ExitUsage {
		t.Errorf("resuming a finished job: exit %d, want %d", code, job.ExitUsage)
	}
	removeFile(t, e.log)
	if code := e.run(t, "resume", jobDir, "--from", "3"); code != 0 {
		t.Fatalf("resume --from 3 by path: exit %d", code)
	}
	if n := len(e.calls(t)); n != 1 {
		t.Errorf("--from 3 ran %d steps, want 1", n)
	}

	running := after
	running.State, running.PID = state.Running, os.Getpid()
	b, _ := json.Marshal(running)
	writeFileT(t, filepath.Join(jobDir, state.FileName), string(b))
	if code := e.run(t, "resume", "origin/b", "--from", "1"); code != job.ExitUsage {
		t.Errorf("resuming a running job: exit %d, want %d", code, job.ExitUsage)
	}
	for _, args := range [][]string{{"resume"}, {"resume", "origin/nope"}, {"resume", "origin/b", "--from", "9"}} {
		if code := e.run(t, args...); code != job.ExitUsage {
			t.Errorf("%v: exit %d, want %d", args, code, job.ExitUsage)
		}
	}
}

func TestClean(t *testing.T) {
	e := newEnv(t, `"repos": {"app": {"url": "{{origin}}", "push": true}}`)
	withFakeGH(t)
	e.captureStderr()
	for _, branch := range []string{"pushed", "unpushed", "dirty", "plain"} {
		args := []string{"run", "--repo", "app", "--branch", branch, "--prompt", "x", "--role", "review"}
		if branch != "pushed" {
			args = append(args, "--push=false")
		}
		if code := e.run(t, args...); code != 0 {
			t.Fatalf("%s: exit %d", branch, code)
		}
	}
	clone := func(job string) string { return filepath.Join(e.root, "work", "app", job, "repo") }
	writeFileT(t, filepath.Join(clone("unpushed"), "new.txt"), "x")
	mustGit(t, clone("unpushed"), "add", "new.txt")
	mustGit(t, clone("unpushed"), "commit", "-q", "-m", "local only")
	writeFileT(t, filepath.Join(clone("dirty"), "README.md"), "changed")
	writeRole(t, e.roles, "review", "FAIL")
	if code := e.run(t, "run", "--repo", "app", "--branch", "failed", "--prompt", "x", "--role", "review"); code != 1 {
		t.Fatalf("failed: exit %d", code)
	}
	exists := func(job string) bool {
		_, err := os.Stat(filepath.Join(e.root, "work", "app", job))
		return err == nil
	}

	_, out := e.runIO(t, "clean")
	for _, want := range []string{"remove  app/pushed", "remove  app/plain", "keep    app/unpushed", "1 commits not on any remote", "keep    app/dirty", "uncommitted changes", "dry run: would free"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "app/failed") || !exists("pushed") {
		t.Errorf("dry run removed something or picked a failed job:\n%s", out)
	}

	if _, out := e.runIO(t, "clean", "--yes"); !strings.Contains(out, "freed") {
		t.Errorf("clean --yes:\n%s", out)
	}
	if exists("pushed") || exists("plain") || !exists("unpushed") || !exists("dirty") || !exists("failed") {
		t.Error("clean --yes removed the wrong jobs")
	}
	e.runIO(t, "clean", "--yes", "--failed", "--force")
	if exists("unpushed") || exists("dirty") || exists("failed") {
		t.Error("--failed --force should remove the rest")
	}

	// A running job is never touched.
	live := filepath.Join(e.root, "work", "app", "live")
	b, _ := json.Marshal(state.Job{Repo: "app", Job: "live", State: state.Running, PID: os.Getpid(), JobDir: live, Started: time.Now(), Updated: time.Now()})
	writeFileT(t, filepath.Join(live, state.FileName), string(b))
	e.runIO(t, "clean", "--yes", "--failed", "--force")
	if !exists("live") {
		t.Error("clean removed a running job")
	}
}
