package opencode

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chrisrob11/prfoundry/internal/agent"
)

// followFile follows the events in testdata/name, captured from opencode
// v2.2.0 (see run.jsonl: a read, a shell call the run rejected, then "ok").
func followFile(t *testing.T, name string) (agent.Result, string, *agent.Progress) {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var raw, feed strings.Builder
	prog := &agent.Progress{}
	res := follow(strings.NewReader(string(b)), agent.Output{Raw: &raw, Feed: &feed, Progress: prog, Dir: "/r"})
	if raw.String() != string(b) {
		t.Errorf("raw copy differs:\n%s", raw.String())
	}
	return res, feed.String(), prog
}

func TestFollowARun(t *testing.T) {
	res, feed, prog := followFile(t, "run.jsonl")
	if want := (agent.Result{Got: true, Text: "ok", Turns: 2}); res != want {
		t.Errorf("result = %+v, want %+v", res, want)
	}
	want := "  [read] a.txt\n  [shell] echo hi (failed: This non-interactive run cannot ask the user for permission, so the request was rejected. Continue without this action.)\n  [opencode] ok\n"
	if feed != want {
		t.Errorf("feed = %q, want %q", feed, want)
	}
	if tools, last := prog.Snapshot(); tools != 2 || last != "[shell] echo hi" {
		t.Errorf("progress = %d, %q", tools, last)
	}
}

func TestFollowAnError(t *testing.T) {
	res, feed, _ := followFile(t, "error.jsonl")
	if want := (agent.Result{Got: true, IsError: true, Text: "Model unavailable: nosuch/model"}); res != want {
		t.Errorf("result = %+v, want %+v", res, want)
	}
	if want := "  [opencode] error (provider.no-route): Model unavailable: nosuch/model\n"; feed != want {
		t.Errorf("feed = %q, want %q", feed, want)
	}
}

func TestFollowKeepsTheLastStepsText(t *testing.T) {
	stream := `{"type":"step_start"}
{"type":"text","part":{"text":"Looking around."}}
{"type":"step_finish","part":{"cost":0.01,"tokens":{"cache":{"read":100}}}}
{"type":"step_start"}
{"type":"text","part":{"text":"Done."}}
{"type":"text","part":{"text":"All tests pass."}}
{"type":"step_finish","part":{"cost":0.02,"tokens":{"cache":{"read":200}}}}
{"type":"step_start"}
`
	res := follow(strings.NewReader(stream), agent.Output{Raw: &strings.Builder{}, Feed: &strings.Builder{}, Progress: &agent.Progress{}})
	if res.Text != "Done.\nAll tests pass." || res.Turns != 3 || res.CacheRead != 300 || res.Cost < 0.0299 || res.Cost > 0.0301 {
		t.Errorf("result = %+v", res)
	}
}

func TestFollowStopsAtTheBudget(t *testing.T) {
	stream := strings.Repeat(`{"type":"step_start"}
{"type":"step_finish","part":{"cost":0.25}}
`, 4)
	stops := 0
	res := follow(strings.NewReader(stream), agent.Output{Raw: &strings.Builder{}, Feed: &strings.Builder{}, Progress: &agent.Progress{},
		Budget: 0.5, Stop: func() { stops++ }})
	if !res.OverBudget || stops != 1 {
		t.Errorf("over budget %t after %d stops, want true after 1", res.OverBudget, stops)
	}
	res = follow(strings.NewReader(stream), agent.Output{Raw: &strings.Builder{}, Feed: &strings.Builder{}, Progress: &agent.Progress{}})
	if res.OverBudget {
		t.Error("over budget with no budget")
	}
}

func TestFollowReadsAUsageLimit(t *testing.T) {
	cases := []struct {
		line    string
		limited bool
		wait    time.Duration
	}{
		{`{"type":"error","error":{"type":"provider.rate-limit","message":"Rate limited","status":429,"retryAfterMs":120000}}`, true, 2 * time.Minute},
		{`{"type":"error","error":{"type":"provider.budget-limit","message":"Workspace usage limit reached."}}`, true, limitFallback},
		{`{"type":"error","error":{"type":"provider.free-limit","message":"Free usage limit reached."}}`, false, 0},
		{`{"type":"error","error":{"type":"provider.auth","message":"bad key"}}`, false, 0},
	}
	for _, c := range cases {
		before := time.Now()
		res := follow(strings.NewReader(c.line), agent.Output{Raw: &strings.Builder{}, Feed: &strings.Builder{}, Progress: &agent.Progress{}})
		if res.Limited != c.limited || !res.IsError {
			t.Errorf("%s: limited %t, error %t", c.line, res.Limited, res.IsError)
		}
		if c.limited && (res.Reset.Before(before.Add(c.wait)) || res.Reset.After(time.Now().Add(c.wait))) {
			t.Errorf("%s: resets at %v, want %v from now", c.line, res.Reset, c.wait)
		}
	}
}

func TestDescribeTool(t *testing.T) {
	for _, c := range []struct {
		name  string
		input map[string]any
		want  string
	}{
		{"shell", map[string]any{"command": "go test ./...\necho done"}, "[shell] go test ./..."},
		{"edit", map[string]any{"path": "/r/internal/db.go"}, "[edit] internal/db.go"},
		{"Grep", map[string]any{"pattern": "func New"}, "[grep] func New"},
		{"todo", map[string]any{}, "[todo]"},
	} {
		if got := describeTool(c.name, c.input, "/r"); got != c.want {
			t.Errorf("describeTool(%s, %v) = %q, want %q", c.name, c.input, got, c.want)
		}
	}
}
