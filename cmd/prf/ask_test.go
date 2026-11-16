package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chrisrob11/prfoundry/internal/pipeline"
)

// ownerSaw is what fakeOwner saw of a question: the page, its URL, and the
// job's state.
type ownerSaw struct {
	reviewView
	url   string
	state jobState
}

// fakeOwner stands in for you at an ask gate. When the gate opens its page,
// it keeps it open, as a browser tab would: for each of answers in turn, it
// waits for the page to show a question, reads it, README.md at HEAD, and
// the job's state.json, and posts the answer. The gate doesn't open a page
// that's still open, so a second question needs no second open.
func fakeOwner(t *testing.T, answers ...string) *[]ownerSaw {
	t.Helper()
	var views []ownerSaw
	opened := false
	prevOpen, prevNotify := openBrowser, notify
	t.Cleanup(func() { openBrowser, notify = prevOpen, prevNotify })
	notify = func(string, string) {}
	openBrowser = func(page string) error {
		if len(answers) == 0 || opened {
			t.Errorf("question page opened again, or with no answers to give: %s", page)
			return nil
		}
		opened = true
		go func() {
			version := -1
			for _, body := range answers {
				saw, ok := awaitQuestion(page, version)
				if !ok {
					t.Errorf("no question came at %s", page)
					return
				}
				if res, err := http.Get(page + "api/file?path=" + url.QueryEscape("README.md")); err != nil {
					t.Errorf("api/file README.md: %v", err)
				} else {
					_ = res.Body.Close()
					if res.StatusCode != http.StatusOK {
						t.Errorf("api/file README.md: %s", res.Status)
					}
				}
				views = append(views, saw)
				res, err := http.Post(page+"api/submit", "application/json", strings.NewReader(body))
				if err != nil {
					t.Errorf("posting answers: %v", err)
					return
				}
				var out struct{ Version int }
				err = json.NewDecoder(res.Body).Decode(&out)
				_ = res.Body.Close()
				if res.StatusCode != http.StatusOK {
					t.Errorf("posting answers: %s", res.Status)
					return
				}
				if err != nil {
					t.Errorf("decoding the answers' reply: %v", err)
					return
				}
				version = out.Version
			}
		}()
		return nil
	}
	return &views
}

// awaitQuestion polls page until it waits for answers at a version after
// version, and the job's state.json says so too.
func awaitQuestion(page string, version int) (ownerSaw, bool) {
	saw := ownerSaw{url: page}
	for range 500 {
		time.Sleep(10 * time.Millisecond)
		res, err := http.Get(page + "api/review")
		if err != nil {
			continue
		}
		saw.reviewView = reviewView{}
		err = json.NewDecoder(res.Body).Decode(&saw.reviewView)
		_ = res.Body.Close()
		if err != nil || saw.State != pageWaiting || saw.Version <= version {
			continue
		}
		if paths, _ := filepath.Glob(filepath.Join("work", "*", "*", stateFile)); len(paths) == 1 {
			b, _ := os.ReadFile(paths[0])
			saw.state = jobState{}
			if err := json.Unmarshal(b, &saw.state); err != nil {
				continue
			}
		}
		if saw.state.State == stateAsk {
			return saw, true
		}
	}
	return saw, false
}

const (
	answer = `{"verdict": "answer", "body": "Go left.", "comments": [
		{"block": 3, "code": "(a) left (Recommended)", "body": "Yes, this one."},
		{"path": "README.md", "line": 1, "side": "new", "body": "And keep this line."}]}`
	acceptAll = `{"verdict": "accept"}`
	stopJob   = `{"verdict": "stop"}`
)

// askEnv is an env whose asker role asks count times, into .agent/stop.md
// in the checkout, before an @ask gate configured by gate (JSON) and then
// the test role.
func askEnv(t *testing.T, count int, gate string) env {
	e := newEnv(t, "")
	captureStderr(t)
	writeRole(t, e.roles, "asker", "ASK on {{.Branch}}")
	writeRole(t, e.roles, pipeline.Step{Gate: pipeline.GateAsk}.Fixer(), "ANSWER on {{.Branch}}\n{{.Question}}\n{{.Review}}")
	asks := filepath.Join(e.root, "asks")
	writeFileT(t, asks, strconv.Itoa(count))
	t.Setenv("FAKE_ASKS", asks)
	t.Setenv("FAKE_ASK_PATH", ".agent/stop.md")
	writeFileT(t, filepath.Join(e.home, "origin", pipeline.FileName), `{"default": "a", "pipelines": {"a": ["asker", `+gate+`, "test"]}}`)
	return e
}

func TestAskGate(t *testing.T) {
	e := askEnv(t, 1, `{"gate": "ask", "question": ".agent/stop.md", "model": "opus"}`)
	views := fakeOwner(t, answer)

	if code := e.run(t, "run", "--repo", e.origin, "--branch", "b", "--prompt", "x"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if len(*views) != 1 {
		t.Fatalf("page opened %d times, want once", len(*views))
	}
	if v := (*views)[0]; v.Kind != pipeline.GateAsk || v.Asker != "asker" || v.QFile != ".agent/stop.md" || !strings.Contains(v.Question, "See README.md:1.") || v.Round != 1 {
		t.Errorf("view = %+v", v)
	}
	st := (*views)[0].state
	if st.State != stateAsk || st.ReviewURL != (*views)[0].url || st.Step != 2 || stageOf(st) != "ask (waiting for owner)" || detailOf(st) != st.ReviewURL || !st.live() {
		t.Errorf("state while waiting = %+v", st)
	}

	calls := e.calls(t)
	var prompts []string
	for _, c := range calls {
		prompts = append(prompts, strings.SplitN(c[1], " ", 3)[1])
	}
	if want := []string{"ASK", "ANSWER", "ASK", "test"}; strings.Join(prompts, ",") != strings.Join(want, ",") {
		t.Fatalf("claude calls start %v, want %v", prompts, want)
	}
	fix := calls[1][1]
	for _, want := range []string{
		"# Which way?", "Round 1: the owner answered the question in `.agent/stop.md`.", "Go left.",
		"### #1: on the question\n\n> (a) left (Recommended)\n\nYes, this one.", "### #2: `README.md` line 1", "--model opus",
	} {
		if !strings.Contains(fix, want) {
			t.Errorf("fix run's args missing %q:\n%s", want, fix)
		}
	}
	rerun := calls[2][1]
	for _, want := range []string{"## Your questions, answered", "### Question 1", "# Which way?", "Go left."} {
		if !strings.Contains(rerun, want) {
			t.Errorf("re-run's args missing %q:\n%s", want, rerun)
		}
	}
	if strings.Contains(rerun, "--model opus") {
		t.Error("the re-run took the gate's model, not the asker's")
	}

	jobDir := filepath.Join(e.root, "work", "origin", "b")
	status := readFile(t, filepath.Join(jobDir, "status"))
	for _, want := range []string{
		"PRF_START branch=b phase=@ask step=2/3 pipeline=a\n",
		"PRF_ASK branch=b phase=@ask step=2/3 round=1 url=http://127.0.0.1:",
		"PRF_ANSWERED branch=b phase=@ask step=2/3 round=1 verdict=answer comments=2",
		"PRF_START branch=b phase=answer-question step=2/3 pipeline=a model=opus",
		"PRF_START branch=b phase=asker step=2/3 pipeline=a\n",
		"PRF_PHASE branch=b phase=@ask step=2/3 exit=0 rounds=1",
		"PRF_DONE branch=b phase=test exit=0",
	} {
		if !strings.Contains(status, want) {
			t.Errorf("status missing %q:\n%s", want, status)
		}
	}
	rounds, err := readRounds(filepath.Join(jobDir, "asks", "step-2"))
	if err != nil || len(rounds) != 1 {
		t.Fatalf("rounds = %+v, %v", rounds, err)
	}
	if r := rounds[0]; r.Verdict != verdictAnswer || !r.Fixed || !r.Rerun || !strings.Contains(r.Question, "Which way?") || len(r.Comments) != 2 || r.Comments[0].Block != 3 {
		t.Errorf("round = %+v", r)
	}
}

func TestAskGateAcceptsRecommendations(t *testing.T) {
	e := askEnv(t, 1, `"@ask"`)
	t.Setenv("FAKE_ASK_PATH", "") // the default: $PRF_TMP/stop.md
	views := fakeOwner(t, acceptAll)

	if code := e.run(t, "run", "--repo", e.origin, "--branch", "b", "--prompt", "x"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	calls := e.calls(t)
	if len(calls) != 4 {
		t.Fatalf("got %d claude calls, want asker, answer, asker, test", len(calls))
	}
	if fix := calls[1][1]; !strings.Contains(fix, acceptText) || !strings.Contains(fix, "Which way?") {
		t.Errorf("fix run's args:\n%s", fix)
	}
	// The question was in the job's own tmp folder, which every step can write to.
	tmp := filepath.Join(e.root, "work", "origin", "b", "tmp")
	if v := (*views)[0]; v.QFile != filepath.Join(tmp, "stop.md") {
		t.Errorf("question file = %s, want it in %s", v.QFile, tmp)
	}
	if !strings.Contains(calls[0][1], "--add-dir "+tmp) {
		t.Errorf("asker's args don't add %s:\n%s", tmp, calls[0][1])
	}
}

func TestAskGateWithoutAQuestion(t *testing.T) {
	e := askEnv(t, 0, `"@ask"`)
	views := fakeOwner(t)
	if code := e.run(t, "run", "--repo", e.origin, "--branch", "b", "--prompt", "x"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if len(*views) != 0 {
		t.Error("the page opened with no question")
	}
	if n := len(e.calls(t)); n != 2 {
		t.Errorf("got %d claude calls, want asker and test", n)
	}
}

func TestAskGateRunsOutOfRounds(t *testing.T) {
	e := askEnv(t, 5, `{"gate": "ask", "question": ".agent/stop.md", "max_rounds": 1}`)
	fakeOwner(t, answer)
	if code := e.run(t, "run", "--repo", e.origin, "--branch", "b", "--prompt", "x"); code != exitAsk {
		t.Fatalf("exit %d, want %d", code, exitAsk)
	}
	if n := len(e.calls(t)); n != 3 {
		t.Errorf("got %d claude calls, want asker, answer, asker", n)
	}
	status := readFile(t, filepath.Join(e.root, "work", "origin", "b", "status"))
	if !strings.Contains(status, "phase=@ask step=2/3 exit=7 rounds=1 stopped=max_rounds") {
		t.Errorf("status:\n%s", status)
	}
}

func TestAskGateStopJob(t *testing.T) {
	e := askEnv(t, 1, `{"gate": "ask", "question": ".agent/stop.md"}`)
	before := fakeOwner(t, stopJob)
	if code := e.run(t, "run", "--repo", e.origin, "--branch", "b", "--prompt", "x"); code != exitOwner {
		t.Fatalf("exit %d, want %d", code, exitOwner)
	}
	if n := len(e.calls(t)); n != 1 {
		t.Errorf("got %d claude calls, want only the asker's", n)
	}
	jobDir := filepath.Join(e.root, "work", "origin", "b")
	if status := readFile(t, filepath.Join(jobDir, "status")); !strings.Contains(status, "exit=8 rounds=1 stopped=owner") {
		t.Errorf("status:\n%s", status)
	}
	if st := readState(t, jobDir); st.State != stateFailed || st.Step != 2 {
		t.Errorf("state = %+v", st)
	}

	// Resuming at the gate asks the same question again; once answered, it
	// re-runs the asker, which it loads from the step before.
	after := fakeOwner(t, answer)
	if code := e.run(t, "resume", "origin/b"); code != 0 {
		t.Fatalf("resume: exit %d", code)
	}
	if len(*after) != 1 || (*after)[0].Round != 2 || (*after)[0].url != (*before)[0].url {
		t.Errorf("resume showed %+v, first %+v", *after, *before)
	}
	if n := len(e.calls(t)); n != 4 {
		t.Errorf("got %d claude calls in all, want asker, then answer, asker, test", n)
	}
	if _, err := os.Stat(filepath.Join(readState(t, jobDir).Dir, ".agent", "stop.md")); err == nil {
		t.Error("the question is still there")
	}
}

func TestAskSubmissions(t *testing.T) {
	dir := t.TempDir()
	srv, err := startReviewServer(dir, "app", "job", func() string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	defer srv.close()
	srv.set(pageWaiting, reviewView{Kind: pipeline.GateAsk, Round: 1})
	post := func(body string) int {
		res, err := http.Post(srv.url+"api/submit", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		return res.StatusCode
	}
	for _, bad := range []string{approve, `{"verdict": "answer", "body": " "}`} {
		if code := post(bad); code != http.StatusBadRequest {
			t.Errorf("post %s: %d, want 400", bad, code)
		}
	}
	if code := post(acceptAll); code != http.StatusOK {
		t.Errorf("accept: %d", code)
	}
	if res, err := http.Get(srv.url + "api/file?path=x"); err != nil {
		t.Errorf("api/file without a checkout: %v", err)
	} else {
		_ = res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("api/file without a checkout: %s", res.Status)
		}
	}
}

func TestAskSteps(t *testing.T) {
	for _, tt := range []struct {
		steps []pipeline.Step
		want  string
	}{
		{[]pipeline.Step{{Gate: pipeline.GateAsk}}, "must follow a role"},
		{[]pipeline.Step{{Role: "implement"}, {Gate: pipeline.GateReview}, {Gate: pipeline.GateAsk}}, "must follow a role"},
	} {
		if err := pipeline.CheckAskers(tt.steps); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("CheckAskers(%+v) = %v", tt.steps, err)
		}
	}
	if err := pipeline.CheckAskers([]pipeline.Step{{Role: "implement"}, {Gate: pipeline.GateAsk}}); err != nil {
		t.Error(err)
	}
	if err := pipeline.CheckGate(pipeline.Step{Gate: pipeline.GateReview, Question: "q.md"}); err == nil || !strings.Contains(err.Error(), "only for the @ask gate") {
		t.Errorf("question on @review: %v", err)
	}
	var s pipeline.Step
	if err := json.Unmarshal([]byte(`{"gate": "ask", "question": "q.md"}`), &s); err != nil || s.Fixer() != "answer-question" {
		t.Errorf("step = %+v, %v", s, err)
	}
	if b, _ := json.Marshal(s); string(b) != `{"gate":"ask","question":"q.md"}` {
		t.Errorf("marshal = %s", b)
	}
	j := &job{dir: "/repo", env: []string{"PRF_TMP=/jobs/a/tmp", "TMPDIR=/repo/.tmp"}}
	if got := j.questionPath(pipeline.DefaultQuestion); got != "/jobs/a/tmp/stop.md" {
		t.Errorf("questionPath = %s", got)
	}
	if got := j.questionPath("$TMPDIR/stop.md"); got != "/repo/.tmp/stop.md" {
		t.Errorf("questionPath = %s", got)
	}
	if got := j.questionPath(".agent/stop.md"); got != "/repo/.agent/stop.md" {
		t.Errorf("questionPath = %s", got)
	}
}

func TestAskGateTwice(t *testing.T) {
	e := askEnv(t, 2, `"@ask"`)
	t.Setenv("FAKE_ASK_PATH", "")
	views := fakeOwner(t, answer, acceptAll)

	// The command line's @ask:model overrides the pipeline.
	if code := e.run(t, "run", "--repo", e.origin, "--branch", "b", "--prompt", "x", "--role", "asker,@ask:sonnet"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if len(*views) != 2 || (*views)[0].url != (*views)[1].url || (*views)[1].Round != 2 || len((*views)[1].Rounds) != 1 {
		t.Fatalf("pages = %+v", *views)
	}
	calls := e.calls(t)
	if len(calls) != 5 {
		t.Fatalf("got %d claude calls, want asker, then answer and asker twice", len(calls))
	}
	if fix := calls[3][1]; !strings.Contains(fix, "Round 2: the owner answered") || !strings.Contains(fix, acceptText) || !strings.Contains(fix, "--model sonnet") {
		t.Errorf("second fix run's args:\n%s", fix)
	}
	last := calls[4][1]
	for _, want := range []string{"### Question 1", "Go left.", "### Question 2", acceptText} {
		if !strings.Contains(last, want) {
			t.Errorf("last re-run's args missing %q:\n%s", want, last)
		}
	}
	if strings.Contains(last, "--model sonnet") {
		t.Error("the re-run took the gate's model")
	}
}

func TestAskGateEmptyQuestion(t *testing.T) {
	e := askEnv(t, 0, `{"gate": "ask", "question": "$ASK_DIR/q.md"}`)
	t.Setenv("ASK_DIR", filepath.Join(e.root, "qdir"))
	writeFileT(t, filepath.Join(e.root, "qdir", "q.md"), " \n\n")
	views := fakeOwner(t)
	if code := e.run(t, "run", "--repo", e.origin, "--branch", "b", "--prompt", "x"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if len(*views) != 0 {
		t.Error("the page opened for an empty question")
	}
}

func TestAskGateFixRoleFails(t *testing.T) {
	e := askEnv(t, 1, `{"gate": "ask", "question": ".agent/stop.md"}`)
	t.Setenv("FAKE_FAIL_ON", "ANSWER on")
	before := fakeOwner(t, answer)
	if code := e.run(t, "run", "--repo", e.origin, "--branch", "b", "--prompt", "x"); code != 1 {
		t.Fatalf("exit %d, want the fix role's 1", code)
	}
	jobDir := filepath.Join(e.root, "work", "origin", "b")
	rounds, _ := readRounds(filepath.Join(jobDir, "asks", "step-2"))
	if len(rounds) != 1 || rounds[0].Fixed || rounds[0].Verdict != verdictAnswer {
		t.Fatalf("rounds = %+v", rounds)
	}

	// Resuming records the saved answers without asking again, then re-runs
	// the asker.
	t.Setenv("FAKE_FAIL_ON", "")
	after := fakeOwner(t)
	if code := e.run(t, "resume", "origin/b"); code != 0 {
		t.Fatalf("resume: exit %d", code)
	}
	if len(*after) != 0 || len(*before) != 1 {
		t.Errorf("resume asked again: %+v", *after)
	}
	calls := e.calls(t)
	if len(calls) != 5 {
		t.Fatalf("got %d claude calls, want asker, answer (failed), answer, asker, test", len(calls))
	}
	if fix := calls[2][1]; !strings.Contains(fix, "Go left.") {
		t.Errorf("resumed fix run's args:\n%s", fix)
	}
	if rounds, _ := readRounds(filepath.Join(jobDir, "asks", "step-2")); len(rounds) != 1 || !rounds[0].Fixed || !rounds[0].Rerun {
		t.Errorf("rounds after resume = %+v", rounds)
	}
}

func TestAskGateRerunFails(t *testing.T) {
	e := askEnv(t, 1, `{"gate": "ask", "question": ".agent/stop.md"}`)
	t.Setenv("FAKE_FAIL_ON", "## Your questions, answered")
	fakeOwner(t, answer)
	if code := e.run(t, "run", "--repo", e.origin, "--branch", "b", "--prompt", "x"); code != 1 {
		t.Fatalf("exit %d, want the re-run's 1", code)
	}
	jobDir := filepath.Join(e.root, "work", "origin", "b")
	if rounds, _ := readRounds(filepath.Join(jobDir, "asks", "step-2")); len(rounds) != 1 || !rounds[0].Fixed || rounds[0].Rerun {
		t.Errorf("rounds = %+v", rounds)
	}
	status := readFile(t, filepath.Join(jobDir, "status"))
	for _, want := range []string{"phase=asker step=2/3 exit=1", "phase=@ask step=2/3 exit=1 rounds=1", "PRF_DONE branch=b phase=@ask exit=1"} {
		if !strings.Contains(status, want) {
			t.Errorf("status missing %q:\n%s", want, status)
		}
	}
}

func TestAskPipelineValidation(t *testing.T) {
	e := newEnv(t, "")
	captureStderr(t)
	for _, roles := range []string{"@ask,implement", "implement,@review,@ask"} {
		if code := e.run(t, "pipeline", "set", "p", "--repo", "origin", "--roles", roles); code != exitUsage {
			t.Errorf("pipeline set %s: exit %d, want %d", roles, code, exitUsage)
		}
	}
	if code := e.run(t, "run", "--repo", e.origin, "--branch", "b", "--prompt", "x", "--role", "@ask"); code != exitUsage {
		t.Errorf("run --role @ask: exit %d, want %d", code, exitUsage)
	}
}

func TestFileAtHead(t *testing.T) {
	j := &job{dir: newOrigin(t)}
	if text, ok := j.fileAtHead("README.md"); !ok || !strings.HasPrefix(text, "# origin") {
		t.Errorf("README.md = %q, %t", text, ok)
	}
	for _, path := range []string{"missing.go", "../outside", "/etc/passwd", "a/../../x"} {
		if _, ok := j.fileAtHead(path); ok {
			t.Errorf("fileAtHead(%q) found a file", path)
		}
	}

	srv, err := startReviewServer(t.TempDir(), "app", "job", func() string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	defer srv.close()
	srv.fileAt = j.fileAtHead
	get := func(path string) (int, map[string]string) {
		res, err := http.Get(srv.url + "api/file?path=" + url.QueryEscape(path))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		var out map[string]string
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return res.StatusCode, out
	}
	if code, out := get("README.md"); code != http.StatusOK || out["path"] != "README.md" || !strings.HasPrefix(out["text"], "# origin") {
		t.Errorf("api/file README.md: %d %v", code, out)
	}
	if code, _ := get("../x"); code != http.StatusNotFound {
		t.Errorf("api/file ../x: %d", code)
	}
}

func TestRenderAnswers(t *testing.T) {
	r := reviewRound{Round: 3, Verdict: verdictAccept, Body: "Also: hurry.", Comments: []reviewComment{
		{ID: 1, Block: 2, Code: "line one\nline two", Body: "This."},
		{ID: 2, Path: "a.go", Body: "Whole file."},
		{ID: 3, Path: "a.go", Line: 4, Side: "old", Code: "-gone", Body: "Why?"},
	}}
	got := renderAnswers(r, "q.md")
	for _, want := range []string{
		"Round 3: the owner answered the question in `q.md`.\n\n" + acceptText + "\n\nAlso: hurry.\n",
		"### #1: on the question\n\n> line one\n> line two\n\nThis.\n",
		"### #2: `a.go`\n\nWhole file.\n",
		"### #3: `a.go` line 4 (a removed line)\n\n```diff\n-gone\n```\n\nWhy?\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("renderAnswers missing %q:\n%s", want, got)
		}
	}
	if renderAnswered([]reviewRound{{Round: 1, Question: "Q?"}}) != "" {
		t.Error("renderAnswered included a round its fix role hadn't recorded")
	}
}

func TestAskStatus(t *testing.T) {
	s := jobState{State: stateAsk, Stage: "@ask", Step: 2, Models: []string{"", "opus"}, ReviewURL: "http://127.0.0.1:1/x/"}
	if got := stageOf(s); got != "ask (waiting for owner)" {
		t.Errorf("stageOf = %q", got)
	}
	if got := detailOf(s); got != s.ReviewURL {
		t.Errorf("detailOf = %q", got)
	}
}

// TestQuestionMarkdown runs the review page's question parser and file:line
// matcher in node, when it's installed.
func TestQuestionMarkdown(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node isn't installed")
	}
	page := string(reviewPage)
	start, end := strings.Index(page, "function mdBlocks"), strings.Index(page, "function withRefs")
	if start < 0 || end < start {
		t.Fatal("review.html no longer has mdBlocks and refPattern before withRefs")
	}
	script := page[start:end] + `
const q = "# Stop: CS1a\n\nThe brief says X but\nscripts/wave/launch-pr.sh:42 does Y.\n\n## Options\n\n" +
  "- (a) keep it (Recommended)\n  more on a\n- (b) drop it, see ` + "`config.go:12-20`" + `\n1. one\n\n> quoted\n\n" +
  "` + "```" + `go\nfunc x() {}\n` + "```" + `\n\n---\nhttp://example.com:80 and README.md:3";
console.log(JSON.stringify({
  blocks: mdBlocks(q).map(b => b.t + ":" + b.text),
  refs: [...q.matchAll(refPattern)].map(m => m[1] + "@" + m[2]),
}));
`
	path := filepath.Join(t.TempDir(), "md.js")
	writeFileT(t, path, script)
	out, err := exec.Command(node, path).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var got struct{ Blocks, Refs []string }
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	wantBlocks := []string{
		"h:Stop: CS1a", "p:The brief says X but\nscripts/wave/launch-pr.sh:42 does Y.", "h:Options",
		"li:(a) keep it (Recommended)\nmore on a", "li:(b) drop it, see `config.go:12-20`", "li:one",
		"quote:quoted", "code:func x() {}", "p:http://example.com:80 and README.md:3",
	}
	if !slices.Equal(got.Blocks, wantBlocks) {
		t.Errorf("blocks = %q\nwant %q", got.Blocks, wantBlocks)
	}
	// example.com:80 matches here; withRefs skips it as part of a URL.
	if want := []string{"scripts/wave/launch-pr.sh@42", "config.go@12", "example.com@80", "README.md@3"}; !slices.Equal(got.Refs, want) {
		t.Errorf("refs = %q, want %q", got.Refs, want)
	}
}
