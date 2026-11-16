package dashboard

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chrisrob11/prfoundry/internal/pipeline"
	"github.com/chrisrob11/prfoundry/internal/review"
	"github.com/chrisrob11/prfoundry/internal/state"
)

// gonePID is a pid no process has.
const gonePID = 1 << 30

func jobNames(rows []Row) string {
	var names []string
	for _, r := range rows {
		names = append(names, r.Repo+"/"+r.Job)
	}
	return strings.Join(names, ",")
}

// TestOverview: jobs waiting on you come first, then live ones, then the
// rest, each most recently updated first; a live job whose process is gone
// is died; the repo and state filters narrow the rows but not the repos.
func TestOverview(t *testing.T) {
	workdir := t.TempDir()
	me := os.Getpid()
	at := func(minutes int) time.Time { return now.Add(-time.Duration(minutes) * time.Minute) }
	exit1 := 1
	for _, j := range []state.Job{
		{Repo: "app", Job: "done", State: state.Done, Updated: at(5), Started: at(65)},
		{Repo: "app", Job: "running", State: state.Running, PID: me, Updated: at(30), Started: at(42), Step: 2,
			Roles: []string{"implement", "test"}, Models: []string{"opus", "sonnet"}, Stage: "test", Activity: "[bash] go test"},
		{Repo: "api", Job: "review", State: state.Review, PID: me, Updated: at(50), ReviewURL: "http://127.0.0.1:1/r/"},
		{Repo: "api", Job: "gone", State: state.Running, PID: gonePID, Updated: at(1)},
		{Repo: "api", Job: "failed", State: state.Failed, Exit: &exit1, Updated: at(10), Agent: "opencode"},
		{Repo: "web", Job: "paused", State: state.Paused, PID: me, Updated: at(40), ResumeAt: now.Add(time.Hour)},
	} {
		writeJob(t, workdir, j)
	}
	writeFile(t, filepath.Join(workdir, "web", "broken", state.FileName), "not json")
	srv := start(t, Options{Workdir: workdir})

	var o Overview
	getJSON(t, srv.URL()+"api/jobs", &o)
	if got, want := jobNames(o.Jobs), "api/review,app/running,web/paused,api/gone,app/done,api/failed"; got != want {
		t.Errorf("order = %s, want %s", got, want)
	}
	if strings.Join(o.Repos, ",") != "api,app,web" || !o.Now.Equal(now) {
		t.Errorf("repos = %v, now = %v", o.Repos, o.Now)
	}
	rows := map[string]Row{}
	for _, r := range o.Jobs {
		rows[r.Job] = r
	}
	if r := rows["gone"]; r.State != state.Died || r.Waiting {
		t.Errorf("gone = %+v, want died", r)
	}
	if r := rows["review"]; !r.Waiting || r.ReviewURL == "" {
		t.Errorf("review = %+v, want waiting with its URL", r)
	}
	if r := rows["running"]; r.Elapsed != "42m0s" || r.Model != "sonnet" || r.Steps != 2 || r.Agent != "claude" {
		t.Errorf("running = %+v, want 42m elapsed on sonnet, step 2 of 2", r)
	}
	if r := rows["done"]; r.Elapsed != "1h0m" {
		t.Errorf("a finished job's elapsed = %q, want start to last update", r.Elapsed)
	}
	if r := rows["failed"]; r.Agent != "opencode" || r.Exit == nil || *r.Exit != 1 || r.Elapsed != "" {
		t.Errorf("failed = %+v", r)
	}

	getJSON(t, srv.URL()+"api/jobs?repo=api", &o)
	if got := jobNames(o.Jobs); got != "api/review,api/gone,api/failed" || len(o.Repos) != 3 {
		t.Errorf("?repo=api = %s, repos %v", got, o.Repos)
	}
	getJSON(t, srv.URL()+"api/jobs?state=died", &o)
	if got := jobNames(o.Jobs); got != "api/gone" {
		t.Errorf("?state=died = %s", got)
	}
	getJSON(t, srv.URL()+"api/jobs?repo=app&state=died", &o)
	if len(o.Jobs) != 0 || o.Jobs == nil {
		t.Errorf("no match = %#v, want []", o.Jobs)
	}

	// --repo: only that repo's jobs, and no other repo's job page.
	only := start(t, Options{Workdir: workdir, Repo: "web"})
	getJSON(t, only.URL()+"api/jobs", &o)
	if jobNames(o.Jobs) != "web/paused" {
		t.Errorf("--repo web = %s", jobNames(o.Jobs))
	}
	if code, _, _ := get(t, only.URL()+"api/jobs/app/done"); code != http.StatusNotFound {
		t.Errorf("--repo web shows app/done: %d", code)
	}
	if code, body, _ := get(t, srv.URL()); code != http.StatusOK || !strings.Contains(body, "prf jobs") {
		t.Errorf("overview page: %d", code)
	}
}

// TestJobDetail: a job's pipeline with its gates and the current step
// marked, its spend, its @check logs, and its review and ask rounds.
func TestJobDetail(t *testing.T) {
	workdir := t.TempDir()
	steps := []pipeline.Step{{Role: "implement"}, {Gate: pipeline.GateCheck, Run: "make test", FixRole: "fix-check"}, {Gate: pipeline.GateReview}, {Role: "polish"}}
	dir := writeJob(t, workdir, state.Job{
		Repo: "app", Job: "login", Branch: "prf/login", Pipeline: "default", State: state.Review, PID: os.Getpid(),
		Roles: []string{"implement", "@check", "@review", "polish"}, Models: []string{"opus", "", "", "haiku"}, Steps: steps,
		Step: 3, Stage: "@review", ReviewURL: "http://127.0.0.1:9/app/login/t/", Cost: 1.5,
		Spend: []state.Spend{{Role: "implement", Model: "opus", Runs: 1, Turns: 20, Cost: 1.2}, {Role: "fix-check", Runs: 1, Turns: 3, Cost: 0.3}},
	})
	writeFile(t, filepath.Join(dir, "checks", "step-2", "round-2.log"), "FAIL again\n")
	writeFile(t, filepath.Join(dir, "checks", "step-2", "round-1.log"), "FAIL\n")
	writeFile(t, filepath.Join(dir, "checks", "step-2", "notes.txt"), "not a round")
	if err := review.WriteRound(filepath.Join(dir, "reviews"), review.Round{Round: 1, Verdict: review.Changes, Body: "rename it",
		Comments: []review.Comment{{ID: 1, Path: "a.go", Line: 3, Body: "why?", Reply: "fixed"}}, Fixed: true}); err != nil {
		t.Fatal(err)
	}
	if err := review.WriteRound(filepath.Join(dir, "asks", "step-1"), review.Round{Round: 1, Verdict: review.Answer, Question: "# Which way?", Body: "left"}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "asks", "step-4", "round-1.json"), "not json")
	srv := start(t, Options{Workdir: workdir})

	var d Detail
	getJSON(t, srv.URL()+"api/jobs/app/login", &d)
	var got []string
	for _, s := range d.Pipe {
		got = append(got, fmt.Sprintf("%d %s %s %s %s", s.N, s.Name, s.Gate, s.Model, s.Status))
	}
	want := []string{"1 implement  opus done", "2 @check check  done", "3 @review review  current", "4 polish  haiku pending"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("steps =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if d.Pipe[1].Run != "make test" || d.Pipe[1].FixRole != "fix-check" {
		t.Errorf("check step = %+v", d.Pipe[1])
	}
	if !d.Waiting || d.Branch != "prf/login" || len(d.Spend) != 2 || d.Spend[0].Cost != 1.2 || d.ReviewURL == "" {
		t.Errorf("detail = %+v", d)
	}
	if len(d.Checks) != 2 || d.Checks[0].Round != 1 || d.Checks[1].Round != 2 || d.Checks[1].Size != 11 {
		t.Errorf("checks = %+v, want rounds 1 and 2 of step 2", d.Checks)
	}
	if len(d.Reviews) != 1 || d.Reviews[0].Comments[0].Reply != "fixed" {
		t.Errorf("reviews = %+v", d.Reviews)
	}
	if len(d.Asks) != 1 || d.Asks[0].Step != 1 || d.Asks[0].Rounds[0].Body != "left" {
		t.Errorf("asks = %+v", d.Asks)
	}

	code, body, kind := get(t, srv.URL()+"api/jobs/app/login/checks/2/2")
	if code != http.StatusOK || body != "FAIL again\n" || !strings.HasPrefix(kind, "text/plain") {
		t.Errorf("check log = %d %q %s", code, body, kind)
	}
	for _, path := range []string{"api/jobs/app/login/checks/2/9", "api/jobs/app/login/checks/x/1", "api/jobs/app/login/checks/0/1",
		"api/jobs/app/nope", "api/jobs/app/%2e%2e", "api/jobs/app/a%2Fb", "api/jobs/app/login/checks/2/..%2f..%2fstate.json"} {
		if code, _, _ := get(t, srv.URL()+path); code != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", path, code)
		}
	}
	if code, body, _ := get(t, srv.URL()+"job/app/login"); code != http.StatusOK || !strings.Contains(body, "claude.log") {
		t.Errorf("job page: %d", code)
	}

	// A job with no steps or spend recorded yet: empty lists, not nulls.
	writeJob(t, workdir, state.Job{Repo: "app", Job: "new", State: state.Setup, PID: os.Getpid(), Stage: "cloning"})
	code, body, _ = get(t, srv.URL()+"api/jobs/app/new")
	if code != http.StatusOK || strings.Contains(body, "null") {
		t.Errorf("a new job = %d %s", code, body)
	}
}

// TestStepStatus: how each step is marked as a job finishes, fails or dies.
func TestStepStatus(t *testing.T) {
	roles := []string{"implement", "@check", "review"}
	status := func(st string, step int) string {
		var out []string
		for _, s := range steps(state.Job{State: st, Step: step, Roles: roles}) {
			out = append(out, s.Status)
		}
		return strings.Join(out, ",")
	}
	for _, c := range []struct {
		state string
		step  int
		want  string
	}{
		{state.Setup, 0, "pending,pending,pending"},
		{state.Running, 1, "current,pending,pending"},
		{state.Done, 3, "done,done,done"},
		{state.Failed, 2, "done,failed,pending"},
		{state.Died, 3, "done,done,failed"},
	} {
		if got := status(c.state, c.step); got != c.want {
			t.Errorf("%s at %d = %s, want %s", c.state, c.step, got, c.want)
		}
	}
	// Without the recorded steps, a gate is read from its name.
	if s := steps(state.Job{Roles: roles}); s[1].Gate != pipeline.GateCheck {
		t.Errorf("@check from its name = %+v", s[1])
	}
}

// TestLogTail: the last MaxLog lines of claude.log, saying when earlier
// ones are left out; a log too big to read whole starts at a full line.
func TestLogTail(t *testing.T) {
	workdir := t.TempDir()
	dir := writeJob(t, workdir, state.Job{Repo: "app", Job: "a", State: state.Done})
	writeJob(t, workdir, state.Job{Repo: "app", Job: "nolog", State: state.Done})
	srv := start(t, Options{Workdir: workdir, MaxLog: 200})
	log := filepath.Join(dir, "claude.log")

	var lines strings.Builder
	for i := 1; i <= 500; i++ {
		fmt.Fprintf(&lines, "line %d\n", i)
	}
	writeFile(t, log, lines.String())
	var tail LogTail
	getJSON(t, srv.URL()+"api/jobs/app/a/log", &tail)
	if len(tail.Lines) != 200 || tail.Lines[0] != "line 301" || tail.Lines[199] != "line 500" || !tail.Truncated {
		t.Errorf("500 lines: got %d, first %q, truncated %t", len(tail.Lines), tail.Lines[0], tail.Truncated)
	}

	writeFile(t, log, "one\ntwo\n")
	getJSON(t, srv.URL()+"api/jobs/app/a/log", &tail)
	if strings.Join(tail.Lines, ",") != "one,two" || tail.Truncated {
		t.Errorf("short log = %q, truncated %t", tail.Lines, tail.Truncated)
	}

	// Bigger than tailBytes: the partial first line is dropped.
	writeFile(t, log, strings.Repeat("x", tailBytes)+"\nlast\n")
	getJSON(t, srv.URL()+"api/jobs/app/a/log", &tail)
	if strings.Join(tail.Lines, ",") != "last" || !tail.Truncated {
		t.Errorf("huge log = %d lines, truncated %t", len(tail.Lines), tail.Truncated)
	}

	writeFile(t, log, "")
	for _, job := range []string{"a", "nolog"} {
		code, body, _ := get(t, srv.URL()+"api/jobs/app/"+job+"/log")
		if code != http.StatusOK || strings.TrimSpace(body) != `{"lines":[],"truncated":false}` {
			t.Errorf("%s: an empty or missing log = %d %s", job, code, body)
		}
	}

	big := filepath.Join(dir, "checks", "step-1", "round-1.log")
	writeFile(t, big, strings.Repeat("y", tailBytes+10))
	code, body, _ := get(t, srv.URL()+"api/jobs/app/a/checks/1/1")
	if code != http.StatusOK || !strings.HasPrefix(body, "[truncated:") || len(body) > tailBytes+100 {
		t.Errorf("a big check log = %d, %d bytes, starts %q", code, len(body), body[:min(len(body), 20)])
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestDiff: git diff <base_commit> in the job's checkout, committed and
// uncommitted, cut at MaxDiff on a line boundary and saying so; and a note
// instead when there's no base, no checkout, or git fails.
func TestDiff(t *testing.T) {
	workdir, repo := t.TempDir(), t.TempDir()
	git(t, repo, "init", "-q")
	writeFile(t, filepath.Join(repo, "a.txt"), "one\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "base")
	base := git(t, repo, "rev-parse", "HEAD")
	writeFile(t, filepath.Join(repo, "a.txt"), "one\ntwo\n")
	git(t, repo, "commit", "-qam", "two")
	writeFile(t, filepath.Join(repo, "a.txt"), "one\ntwo\n<three>\n")
	writeJob(t, workdir, state.Job{Repo: "app", Job: "small", State: state.Done, Dir: repo, BaseCommit: base})
	writeJob(t, workdir, state.Job{Repo: "app", Job: "nobase", State: state.Done, Dir: repo})
	writeJob(t, workdir, state.Job{Repo: "app", Job: "gone", State: state.Done, Dir: filepath.Join(repo, "nope"), BaseCommit: base})
	writeJob(t, workdir, state.Job{Repo: "app", Job: "badbase", State: state.Done, Dir: repo, BaseCommit: "0123456789abcdef0123456789abcdef01234567"})
	srv := start(t, Options{Workdir: workdir, MaxDiff: 4096})

	var d Diff
	getJSON(t, srv.URL()+"api/jobs/app/small/diff", &d)
	if !strings.Contains(d.Text, "+two\n+<three>\n") || d.Truncated || d.Note != "" || d.Base != base {
		t.Errorf("small diff = %+v", d)
	}
	for job, note := range map[string]string{"nobase": "no base commit", "gone": "checkout is gone", "badbase": "git diff 0123456789"} {
		d = Diff{}
		getJSON(t, srv.URL()+"api/jobs/app/"+job+"/diff", &d)
		if !strings.Contains(d.Note, note) || d.Text != "" {
			t.Errorf("%s: diff = %+v, want a note saying %q", job, d, note)
		}
	}

	var big strings.Builder
	for i := range 2000 {
		fmt.Fprintf(&big, "added line %d\n", i)
	}
	writeFile(t, filepath.Join(repo, "a.txt"), big.String())
	d = Diff{}
	getJSON(t, srv.URL()+"api/jobs/app/small/diff", &d)
	if !d.Truncated || len(d.Text) > 4096 || len(d.Text) < 4000 || !strings.HasSuffix(d.Text, "\n") || d.Note != "" {
		t.Errorf("big diff: truncated %t, %d bytes, note %q", d.Truncated, len(d.Text), d.Note)
	}
}
