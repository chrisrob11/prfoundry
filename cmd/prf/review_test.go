package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chrisrob11/prfoundry/internal/pipeline"
	"github.com/chrisrob11/prfoundry/internal/state"
)

// fakeReviewer stands in for you at a review gate: each time the gate opens
// the review page, it posts the next of reviews to it.
func fakeReviewer(t *testing.T, reviews ...string) *[]string {
	t.Helper()
	var urls []string
	prevOpen, prevNotify := openBrowser, notify
	t.Cleanup(func() { openBrowser, notify = prevOpen, prevNotify })
	notify = func(string, string) {}
	openBrowser = func(url string) error {
		urls = append(urls, url)
		if len(urls) > len(reviews) {
			t.Errorf("review page opened %d times, want %d", len(urls), len(reviews))
			return nil
		}
		body := reviews[len(urls)-1]
		go func() {
			res, err := http.Post(url+"api/submit", "application/json", strings.NewReader(body))
			if err != nil {
				t.Errorf("posting review: %v", err)
				return
			}
			_ = res.Body.Close()
			if res.StatusCode != http.StatusOK {
				t.Errorf("posting review: %s", res.Status)
			}
		}()
		return nil
	}
	return &urls
}

const (
	approve        = `{"verdict": "approve"}`
	requestChanges = `{"verdict": "changes", "body": "Close, but not yet.", "comments": [
		{"path": "README.md", "line": 2, "side": "new", "code": " # origin\n+more", "body": "Say more here."},
		{"path": "README.md", "body": "  "}]}`
)

func TestReviewGate(t *testing.T) {
	e := newEnv(t, "")
	captureStderr(t)
	writeRole(t, e.roles, pipeline.Step{Gate: pipeline.GateReview}.Fixer(), "fix on {{.Branch}}\n{{.Review}}")
	urls := fakeReviewer(t, requestChanges, approve)

	if code := e.run(t, "run", "--repo", e.origin, "--branch", "b", "--prompt", "the task", "--role", "implement,@review:opus"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if len(*urls) != 2 || (*urls)[0] != (*urls)[1] || !strings.Contains((*urls)[0], "/origin/b/") {
		t.Errorf("page opened at %v; want the same page twice, once per round", *urls)
	}

	calls := e.calls(t)
	if len(calls) != 2 {
		t.Fatalf("got %d claude calls, want implement and one fix run: %v", len(calls), calls)
	}
	fix := calls[1][1]
	for _, want := range []string{"fix on b", "Round 1: the reviewer requested changes.", "Close, but not yet.", "### #1: `README.md` line 2", "```diff\n # origin\n+more\n```", "Say more here.", "--model opus"} {
		if !strings.Contains(fix, want) {
			t.Errorf("fix run's args missing %q:\n%s", want, fix)
		}
	}
	if strings.Contains(fix, "#2") {
		t.Error("an empty comment reached the fix role")
	}

	jobDir := filepath.Join(e.root, "work", "origin", "b")
	status := readFile(t, filepath.Join(jobDir, "status"))
	for _, want := range []string{
		"PRF_START branch=b phase=@review step=2/2 pipeline=roles\n",
		"PRF_REVIEW branch=b phase=@review step=2/2 round=1 url=http://127.0.0.1:",
		"PRF_REVIEWED branch=b phase=@review step=2/2 round=1 verdict=changes comments=1",
		"PRF_START branch=b phase=address-review step=2/2 pipeline=roles model=opus",
		"PRF_REVIEW branch=b phase=@review step=2/2 round=2 ",
		"PRF_REVIEWED branch=b phase=@review step=2/2 round=2 verdict=approve comments=0",
		"PRF_PHASE branch=b phase=@review step=2/2 exit=0 rounds=2",
		"PRF_DONE branch=b phase=@review exit=0",
	} {
		if !strings.Contains(status, want) {
			t.Errorf("status missing %q:\n%s", want, status)
		}
	}

	rounds, err := readRounds(filepath.Join(jobDir, "reviews"))
	if err != nil || len(rounds) != 2 {
		t.Fatalf("rounds = %+v, %v", rounds, err)
	}
	if r := rounds[0]; r.Verdict != verdictChanges || !r.Fixed || r.Summary != "did the thing" || len(r.Comments) != 1 || r.Comments[0].ID != 1 || r.Commit == "" {
		t.Errorf("round 1 = %+v", r)
	}
	if r := rounds[1]; r.Round != 2 || r.Verdict != verdictApprove || r.Fixed {
		t.Errorf("round 2 = %+v", r)
	}
	if md := readFile(t, filepath.Join(jobDir, "reviews", "round-1.md")); !strings.Contains(md, "Say more here.") {
		t.Errorf("round-1.md = %q", md)
	}
	st := readState(t, jobDir)
	if st.State != state.Done || st.ReviewURL != "" || st.BaseCommit == "" || len(st.Steps) != 2 || st.Steps[1].Gate != pipeline.GateReview {
		t.Errorf("state = %+v", st)
	}
}

func TestReviewGateRunsOutOfRounds(t *testing.T) {
	e := newEnv(t, "")
	captureStderr(t)
	writeRole(t, e.roles, pipeline.Step{Gate: pipeline.GateReview}.Fixer(), "fix {{.Review}}")
	writeFileT(t, filepath.Join(e.home, "origin", pipeline.FileName), `{"default": "r", "pipelines": {"r": ["implement", {"gate": "review", "max_rounds": 1}]}}`)
	before := fakeReviewer(t, requestChanges, requestChanges)

	if code := e.run(t, "run", "--repo", e.origin, "--branch", "b", "--prompt", "x"); code != exitReview {
		t.Fatalf("exit %d, want %d", code, exitReview)
	}
	if n := len(e.calls(t)); n != 2 {
		t.Errorf("got %d claude calls, want implement and one fix run", n)
	}
	jobDir := filepath.Join(e.root, "work", "origin", "b")
	if status := readFile(t, filepath.Join(jobDir, "status")); !strings.Contains(status, "phase=@review step=2/2 exit=6 rounds=2 stopped=max_rounds") {
		t.Errorf("status:\n%s", status)
	}

	// Resuming picks up round 2's unaddressed changes, then waits for round 3.
	after := fakeReviewer(t, approve)
	if code := e.run(t, "resume", "origin/b"); code != 0 {
		t.Fatalf("resume: exit %d", code)
	}
	if len(*after) != 1 || (*after)[0] != (*before)[0] {
		t.Errorf("review page moved from %v to %v on resume", *before, *after)
	}
	if n := len(e.calls(t)); n != 3 {
		t.Errorf("resume made %d claude calls in all, want one more fix run", n)
	}
	if rounds, _ := readRounds(filepath.Join(jobDir, "reviews")); len(rounds) != 3 || !rounds[1].Fixed || rounds[2].Verdict != verdictApprove {
		t.Errorf("rounds after resume = %+v", rounds)
	}
}

func TestReviewServer(t *testing.T) {
	dir := t.TempDir()
	srv, err := startReviewServer(dir, "app", "my job", func() string { return "Edit main.go" })
	if err != nil {
		t.Fatal(err)
	}
	defer srv.close()
	if !strings.Contains(srv.url, "/app/my-job/") {
		t.Errorf("url %s doesn't name the job", srv.url)
	}
	post := func(body string) int {
		res, err := http.Post(srv.url+"api/submit", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		return res.StatusCode
	}

	if code := post(approve); code != http.StatusConflict {
		t.Errorf("review before the round is ready: %d", code)
	}
	srv.set(pageWaiting, reviewView{Round: 1})
	for _, bad := range []string{`{"verdict": "lgtm"}`, `{"verdict": "changes", "body": " "}`, `not json`} {
		if code := post(bad); code != http.StatusBadRequest {
			t.Errorf("post %s: %d, want 400", bad, code)
		}
	}
	if code := post(requestChanges); code != http.StatusOK {
		t.Fatalf("post: %d", code)
	}
	if code := post(approve); code != http.StatusConflict {
		t.Errorf("second review of a round: %d, want 409", code)
	}
	select {
	case r := <-srv.submitted:
		if r.Verdict != verdictChanges || len(r.Comments) != 1 || r.Comments[0].ID != 1 {
			t.Errorf("submitted %+v", r)
		}
	case <-time.After(time.Second):
		t.Fatal("nothing submitted")
	}

	srv.set(pageFixing, reviewView{Round: 1})
	res, err := http.Get(srv.url + "api/state")
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	err = json.NewDecoder(res.Body).Decode(&state)
	_ = res.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if state["state"] != pageFixing || state["activity"] != "Edit main.go" || !srv.watched() {
		t.Errorf("state = %v, watched = %t", state, srv.watched())
	}

	for _, path := range []string{"/", "/api/state", strings.TrimSuffix(srv.url, "/") + "x/"} {
		url := path
		if strings.HasPrefix(path, "/") {
			url = "http://" + strings.Split(srv.url, "/")[2] + path
		}
		if res, err := http.Get(url); err == nil {
			_ = res.Body.Close()
			if res.StatusCode != http.StatusNotFound {
				t.Errorf("GET %s without the token: %d", url, res.StatusCode)
			}
		}
	}
	res, err = http.Get(srv.url)
	if err != nil {
		t.Fatal(err)
	}
	var page bytes.Buffer
	_, err = page.ReadFrom(res.Body)
	_ = res.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(page.String(), "prf review") {
		t.Error("page not served")
	}
}

func TestReviewLinkIsStable(t *testing.T) {
	dir := t.TempDir()
	first, err := startReviewServer(dir, "app", "login", func() string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	first.close()
	again, err := startReviewServer(dir, "app", "login", func() string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if again.url != first.url {
		t.Errorf("restarted at %s, want %s", again.url, first.url)
	}

	// While that port is taken, a new one keeps the token.
	other, err := startReviewServer(dir, "app", "login", func() string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	defer other.close()
	again.close()
	token := func(url string) string {
		parts := strings.Split(strings.TrimSuffix(url, "/"), "/")
		return parts[len(parts)-1]
	}
	if other.url == again.url || token(other.url) != token(again.url) {
		t.Errorf("with the port taken: %s (was %s)", other.url, again.url)
	}
}

func TestAddReplies(t *testing.T) {
	r := reviewRound{Comments: []reviewComment{{ID: 1}, {ID: 2}, {ID: 3}}}
	addReplies(&r, "Done.\n#1: Renamed it.\n- #3: Kept it: callers need it.\n#9: no such comment\nnot #2: a reply")
	got := []string{r.Comments[0].Reply, r.Comments[1].Reply, r.Comments[2].Reply}
	if want := []string{"Renamed it.", "", "Kept it: callers need it."}; !slices.Equal(got, want) {
		t.Errorf("replies = %q, want %q", got, want)
	}
}
