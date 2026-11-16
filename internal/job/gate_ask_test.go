package job

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/chrisrob11/prfoundry/internal/pipeline"
	"github.com/chrisrob11/prfoundry/internal/review"
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
	defer srv.Close()
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
