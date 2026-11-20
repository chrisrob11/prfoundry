package job

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chrisrob11/prfoundry/internal/pipeline"
	"github.com/chrisrob11/prfoundry/internal/review"
	"github.com/chrisrob11/prfoundry/internal/state"
)

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
	j := &Job{dir: "/repo", env: []string{"PRF_TMP=/jobs/a/tmp", "TMPDIR=/repo/.tmp"}}
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

func TestFileAtHead(t *testing.T) {
	j := &Job{dir: newOrigin(t)}
	if text, ok := j.fileAtHead("README.md"); !ok || !strings.HasPrefix(text, "# origin") {
		t.Errorf("README.md = %q, %t", text, ok)
	}
	for _, path := range []string{"missing.go", "../outside", "/etc/passwd", "a/../../x"} {
		if _, ok := j.fileAtHead(path); ok {
			t.Errorf("fileAtHead(%q) found a file", path)
		}
	}

	srv, err := review.Start(review.Options{Dir: t.TempDir(), Repo: "app", Job: "job", Activity: func() string { return "" }, FileAt: j.fileAtHead, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close(t.Context())
	get := func(path string) (int, map[string]string) {
		res, err := http.Get(srv.URL() + "api/file?path=" + url.QueryEscape(path))
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

// ownerSaw is what fakeOwner saw of a question: the page, its URL, and the
// job's state.
type ownerSaw struct {
	review.View
	url   string
	state state.Job
}

// fakeOwner stands in for you at an ask gate. When the gate opens its page,
// it keeps it open, as a browser tab would: for each of answers in turn, it
// waits for the page to show a question, reads it, README.md at HEAD, and
// the job's state.json, and posts the answer. The gate doesn't open a page
// that's still open, so a second question needs no second open.
func fakeOwner(t *testing.T, tj *testJob, answers ...string) *[]ownerSaw {
	t.Helper()
	var views []ownerSaw
	opened := false
	tj.o.Runtime.Notify = func(string, string) {}
	tj.o.Runtime.OpenBrowser = func(page string) error {
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
		saw.View = review.View{}
		err = json.NewDecoder(res.Body).Decode(&saw.View)
		_ = res.Body.Close()
		if err != nil || saw.State != review.PageWaiting || saw.Version <= version {
			continue
		}
		if paths, _ := filepath.Glob(filepath.Join("work", "*", "*", state.FileName)); len(paths) == 1 {
			b, _ := os.ReadFile(paths[0])
			saw.state = state.Job{}
			if err := json.Unmarshal(b, &saw.state); err != nil {
				continue
			}
		}
		if saw.state.State == state.Ask {
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

// askJob is a job whose asker role asks count times, into .agent/stop.md
// in the checkout, before an @ask gate configured by gate (JSON) and then
// the test role.
func askJob(t *testing.T, count int, gate string) *testJob {
	tj := newTestJob(t, parseSteps(t, `["asker", `+gate+`, "test"]`)...)
	tj.o.Pipeline, tj.o.Task = "a", "x"
	writeRole(t, tj.roles, "asker", "ASK on {{.Branch}}")
	writeRole(t, tj.roles, pipeline.Step{Gate: pipeline.GateAsk}.Fixer(), "ANSWER on {{.Branch}}\n{{.Question}}\n{{.Review}}")
	asks := filepath.Join(tj.root, "asks")
	writeFile(t, asks, strconv.Itoa(count))
	t.Setenv("FAKE_ASKS", asks)
	t.Setenv("FAKE_ASK_PATH", ".agent/stop.md")
	return tj
}

func TestAskGate(t *testing.T) {
	tj := askJob(t, 1, `{"gate": "ask", "question": ".agent/stop.md", "model": "opus"}`)
	views := fakeOwner(t, tj, answer)

	if code := tj.run(t); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if len(*views) != 1 {
		t.Fatalf("page opened %d times, want once", len(*views))
	}
	if v := (*views)[0]; v.Kind != pipeline.GateAsk || v.Asker != "asker" || v.QFile != ".agent/stop.md" || !strings.Contains(v.Question, "See README.md:1.") || v.Round != 1 {
		t.Errorf("view = %+v", v)
	}
	st := (*views)[0].state
	if st.State != state.Ask || st.ReviewURL != (*views)[0].url || st.Step != 2 || !st.Live() {
		t.Errorf("state while waiting = %+v", st)
	}

	calls := tj.calls(t)
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

	jobDir := tj.jobDir()
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
	rounds, err := review.ReadRounds(filepath.Join(jobDir, "asks", "step-2"))
	if err != nil || len(rounds) != 1 {
		t.Fatalf("rounds = %+v, %v", rounds, err)
	}
	if r := rounds[0]; r.Verdict != review.Answer || !r.Fixed || !r.Rerun || !strings.Contains(r.Question, "Which way?") || len(r.Comments) != 2 || r.Comments[0].Block != 3 {
		t.Errorf("round = %+v", r)
	}
}

func TestAskGateAcceptsRecommendations(t *testing.T) {
	tj := askJob(t, 1, `"@ask"`)
	t.Setenv("FAKE_ASK_PATH", "") // the default: $PRF_TMP/stop.md
	views := fakeOwner(t, tj, acceptAll)

	if code := tj.run(t); code != 0 {
		t.Fatalf("exit %d", code)
	}
	calls := tj.calls(t)
	if len(calls) != 4 {
		t.Fatalf("got %d claude calls, want asker, answer, asker, test", len(calls))
	}
	if fix := calls[1][1]; !strings.Contains(fix, review.AcceptText) || !strings.Contains(fix, "Which way?") {
		t.Errorf("fix run's args:\n%s", fix)
	}
	// The question was in the job's own tmp folder, which every step can write to.
	tmp := filepath.Join(tj.jobDir(), "tmp")
	if v := (*views)[0]; v.QFile != filepath.Join(tmp, "stop.md") {
		t.Errorf("question file = %s, want it in %s", v.QFile, tmp)
	}
	if !strings.Contains(calls[0][1], "--add-dir "+tmp) {
		t.Errorf("asker's args don't add %s:\n%s", tmp, calls[0][1])
	}
}

func TestAskGateWithoutAQuestion(t *testing.T) {
	tj := askJob(t, 0, `"@ask"`)
	views := fakeOwner(t, tj)
	if code := tj.run(t); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if len(*views) != 0 {
		t.Error("the page opened with no question")
	}
	if n := len(tj.calls(t)); n != 2 {
		t.Errorf("got %d claude calls, want asker and test", n)
	}
}

func TestAskGateRunsOutOfRounds(t *testing.T) {
	tj := askJob(t, 5, `{"gate": "ask", "question": ".agent/stop.md", "max_rounds": 1}`)
	fakeOwner(t, tj, answer)
	if code := tj.run(t); code != ExitAsk {
		t.Fatalf("exit %d, want %d", code, ExitAsk)
	}
	if n := len(tj.calls(t)); n != 3 {
		t.Errorf("got %d claude calls, want asker, answer, asker", n)
	}
	status := readFile(t, filepath.Join(tj.jobDir(), "status"))
	if !strings.Contains(status, "phase=@ask step=2/3 exit=7 rounds=1 stopped=max_rounds") {
		t.Errorf("status:\n%s", status)
	}
}

func TestAskGateStopJob(t *testing.T) {
	tj := askJob(t, 1, `{"gate": "ask", "question": ".agent/stop.md"}`)
	before := fakeOwner(t, tj, stopJob)
	if code := tj.run(t); code != ExitOwner {
		t.Fatalf("exit %d, want %d", code, ExitOwner)
	}
	if n := len(tj.calls(t)); n != 1 {
		t.Errorf("got %d claude calls, want only the asker's", n)
	}
	jobDir := tj.jobDir()
	if status := readFile(t, filepath.Join(jobDir, "status")); !strings.Contains(status, "exit=8 rounds=1 stopped=owner") {
		t.Errorf("status:\n%s", status)
	}
	if st := readState(t, jobDir); st.State != state.Failed || st.Step != 2 {
		t.Errorf("state = %+v", st)
	}

	// Resuming at the gate asks the same question again; once answered, it
	// re-runs the asker, which it loads from the step before.
	after := fakeOwner(t, tj, answer)
	if code := tj.resume(t); code != 0 {
		t.Fatalf("resume: exit %d", code)
	}
	if len(*after) != 1 || (*after)[0].Round != 2 || (*after)[0].url != (*before)[0].url {
		t.Errorf("resume showed %+v, first %+v", *after, *before)
	}
	if n := len(tj.calls(t)); n != 4 {
		t.Errorf("got %d claude calls in all, want asker, then answer, asker, test", n)
	}
	if _, err := os.Stat(filepath.Join(readState(t, jobDir).Dir, ".agent", "stop.md")); err == nil {
		t.Error("the question is still there")
	}
}

func TestAskGateTwice(t *testing.T) {
	tj := askJob(t, 2, `"@ask"`)
	t.Setenv("FAKE_ASK_PATH", "")
	views := fakeOwner(t, tj, answer, acceptAll)

	// A gate's model, as @ask:model, is for its fix role alone.
	steps, err := pipeline.ParseSteps("asker,@ask:sonnet")
	if err != nil {
		t.Fatal(err)
	}
	tj.o.Steps = steps
	if code := tj.run(t); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if len(*views) != 2 || (*views)[0].url != (*views)[1].url || (*views)[1].Round != 2 || len((*views)[1].Rounds) != 1 {
		t.Fatalf("pages = %+v", *views)
	}
	calls := tj.calls(t)
	if len(calls) != 5 {
		t.Fatalf("got %d claude calls, want asker, then answer and asker twice", len(calls))
	}
	if fix := calls[3][1]; !strings.Contains(fix, "Round 2: the owner answered") || !strings.Contains(fix, review.AcceptText) || !strings.Contains(fix, "--model sonnet") {
		t.Errorf("second fix run's args:\n%s", fix)
	}
	last := calls[4][1]
	for _, want := range []string{"### Question 1", "Go left.", "### Question 2", review.AcceptText} {
		if !strings.Contains(last, want) {
			t.Errorf("last re-run's args missing %q:\n%s", want, last)
		}
	}
	if strings.Contains(last, "--model sonnet") {
		t.Error("the re-run took the gate's model")
	}
}

func TestAskGateEmptyQuestion(t *testing.T) {
	tj := askJob(t, 0, `{"gate": "ask", "question": "$ASK_DIR/q.md"}`)
	t.Setenv("ASK_DIR", filepath.Join(tj.root, "qdir"))
	writeFile(t, filepath.Join(tj.root, "qdir", "q.md"), " \n\n")
	views := fakeOwner(t, tj)
	if code := tj.run(t); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if len(*views) != 0 {
		t.Error("the page opened for an empty question")
	}
}

func TestAskGateFixRoleFails(t *testing.T) {
	tj := askJob(t, 1, `{"gate": "ask", "question": ".agent/stop.md"}`)
	t.Setenv("FAKE_FAIL_ON", "ANSWER on")
	before := fakeOwner(t, tj, answer)
	if code := tj.run(t); code != 1 {
		t.Fatalf("exit %d, want the fix role's 1", code)
	}
	jobDir := tj.jobDir()
	rounds, _ := review.ReadRounds(filepath.Join(jobDir, "asks", "step-2"))
	if len(rounds) != 1 || rounds[0].Fixed || rounds[0].Verdict != review.Answer {
		t.Fatalf("rounds = %+v", rounds)
	}

	// Resuming records the saved answers without asking again, then re-runs
	// the asker.
	t.Setenv("FAKE_FAIL_ON", "")
	after := fakeOwner(t, tj)
	if code := tj.resume(t); code != 0 {
		t.Fatalf("resume: exit %d", code)
	}
	if len(*after) != 0 || len(*before) != 1 {
		t.Errorf("resume asked again: %+v", *after)
	}
	calls := tj.calls(t)
	if len(calls) != 5 {
		t.Fatalf("got %d claude calls, want asker, answer (failed), answer, asker, test", len(calls))
	}
	if fix := calls[2][1]; !strings.Contains(fix, "Go left.") {
		t.Errorf("resumed fix run's args:\n%s", fix)
	}
	if rounds, _ := review.ReadRounds(filepath.Join(jobDir, "asks", "step-2")); len(rounds) != 1 || !rounds[0].Fixed || !rounds[0].Rerun {
		t.Errorf("rounds after resume = %+v", rounds)
	}
}

func TestAskGateRerunFails(t *testing.T) {
	tj := askJob(t, 1, `{"gate": "ask", "question": ".agent/stop.md"}`)
	t.Setenv("FAKE_FAIL_ON", "## Your questions, answered")
	fakeOwner(t, tj, answer)
	if code := tj.run(t); code != 1 {
		t.Fatalf("exit %d, want the re-run's 1", code)
	}
	jobDir := tj.jobDir()
	if rounds, _ := review.ReadRounds(filepath.Join(jobDir, "asks", "step-2")); len(rounds) != 1 || !rounds[0].Fixed || rounds[0].Rerun {
		t.Errorf("rounds = %+v", rounds)
	}
	status := readFile(t, filepath.Join(jobDir, "status"))
	for _, want := range []string{"phase=asker step=2/3 exit=1", "phase=@ask step=2/3 exit=1 rounds=1", "PRF_DONE branch=b phase=@ask exit=1"} {
		if !strings.Contains(status, want) {
			t.Errorf("status missing %q:\n%s", want, status)
		}
	}
}
