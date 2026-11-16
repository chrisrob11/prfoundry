package review

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chrisrob11/prfoundry/internal/pipeline"
)

const (
	approve        = `{"verdict": "approve"}`
	requestChanges = `{"verdict": "changes", "body": "Close, but not yet.", "comments": [
		{"path": "README.md", "line": 2, "side": "new", "code": " # origin\n+more", "body": "Say more here."},
		{"path": "README.md", "body": "  "}]}`
	acceptAll = `{"verdict": "accept"}`
)

func TestReviewServer(t *testing.T) {
	dir := t.TempDir()
	srv, err := Start(Options{Dir: dir, Repo: "app", Job: "my job", Activity: func() string { return "Edit main.go" }, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close(t.Context())
	if !strings.Contains(srv.URL(), "/app/my-job/") {
		t.Errorf("url %s doesn't name the job", srv.URL())
	}
	post := func(body string) int {
		res, err := http.Post(srv.URL()+"api/submit", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		return res.StatusCode
	}

	if code := post(approve); code != http.StatusConflict {
		t.Errorf("review before the round is ready: %d", code)
	}
	srv.Set(PageWaiting, View{Round: 1})
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
	case r := <-srv.Submitted():
		if r.Verdict != Changes || len(r.Comments) != 1 || r.Comments[0].ID != 1 {
			t.Errorf("submitted %+v", r)
		}
	case <-time.After(time.Second):
		t.Fatal("nothing submitted")
	}

	srv.Set(PageFixing, View{Round: 1})
	res, err := http.Get(srv.URL() + "api/state")
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	err = json.NewDecoder(res.Body).Decode(&state)
	_ = res.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if state["state"] != PageFixing || state["activity"] != "Edit main.go" || !srv.Watched() {
		t.Errorf("state = %v, watched = %t", state, srv.Watched())
	}

	for _, path := range []string{"/", "/api/state", strings.TrimSuffix(srv.URL(), "/") + "x/"} {
		url := path
		if strings.HasPrefix(path, "/") {
			url = "http://" + strings.Split(srv.URL(), "/")[2] + path
		}
		if res, err := http.Get(url); err == nil {
			_ = res.Body.Close()
			if res.StatusCode != http.StatusNotFound {
				t.Errorf("GET %s without the token: %d", url, res.StatusCode)
			}
		}
	}
	res, err = http.Get(srv.URL())
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
	first, err := Start(Options{Dir: dir, Repo: "app", Job: "login", Activity: func() string { return "" }, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	first.Close(t.Context())
	again, err := Start(Options{Dir: dir, Repo: "app", Job: "login", Activity: func() string { return "" }, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	if again.URL() != first.URL() {
		t.Errorf("restarted at %s, want %s", again.URL(), first.URL())
	}

	// While that port is taken, a new one keeps the token.
	other, err := Start(Options{Dir: dir, Repo: "app", Job: "login", Activity: func() string { return "" }, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close(t.Context())
	again.Close(t.Context())
	token := func(url string) string {
		parts := strings.Split(strings.TrimSuffix(url, "/"), "/")
		return parts[len(parts)-1]
	}
	if other.URL() == again.URL() || token(other.URL()) != token(again.URL()) {
		t.Errorf("with the port taken: %s (was %s)", other.URL(), again.URL())
	}
}

func TestAddReplies(t *testing.T) {
	r := Round{Comments: []Comment{{ID: 1}, {ID: 2}, {ID: 3}}}
	r.AddReplies("Done.\n#1: Renamed it.\n- #3: Kept it: callers need it.\n#9: no such comment\nnot #2: a reply")
	got := []string{r.Comments[0].Reply, r.Comments[1].Reply, r.Comments[2].Reply}
	if want := []string{"Renamed it.", "", "Kept it: callers need it."}; !slices.Equal(got, want) {
		t.Errorf("replies = %q, want %q", got, want)
	}
}

func TestAskSubmissions(t *testing.T) {
	dir := t.TempDir()
	srv, err := Start(Options{Dir: dir, Repo: "app", Job: "job", Activity: func() string { return "" }, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close(t.Context())
	srv.Set(PageWaiting, View{Kind: pipeline.GateAsk, Round: 1})
	post := func(body string) int {
		res, err := http.Post(srv.URL()+"api/submit", "application/json", strings.NewReader(body))
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
	if res, err := http.Get(srv.URL() + "api/file?path=x"); err != nil {
		t.Errorf("api/file without a checkout: %v", err)
	} else {
		_ = res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("api/file without a checkout: %s", res.Status)
		}
	}
}

func TestRenderAnswers(t *testing.T) {
	r := Round{Round: 3, Verdict: accept, Body: "Also: hurry.", Comments: []Comment{
		{ID: 1, Block: 2, Code: "line one\nline two", Body: "This."},
		{ID: 2, Path: "a.go", Body: "Whole file."},
		{ID: 3, Path: "a.go", Line: 4, Side: "old", Code: "-gone", Body: "Why?"},
	}}
	got := RenderAnswers(r, "q.md")
	for _, want := range []string{
		"Round 3: the owner answered the question in `q.md`.\n\n" + AcceptText + "\n\nAlso: hurry.\n",
		"### #1: on the question\n\n> line one\n> line two\n\nThis.\n",
		"### #2: `a.go`\n\nWhole file.\n",
		"### #3: `a.go` line 4 (a removed line)\n\n```diff\n-gone\n```\n\nWhy?\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("RenderAnswers missing %q:\n%s", want, got)
		}
	}
	if RenderAnswered([]Round{{Round: 1, Question: "Q?"}}) != "" {
		t.Error("RenderAnswered included a round its fix role hadn't recorded")
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
	if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
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
