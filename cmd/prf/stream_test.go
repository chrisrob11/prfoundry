package main

import (
	"strings"
	"testing"
	"time"
)

func TestDescribeTool(t *testing.T) {
	dir := "/work/app/login/repo"
	tests := []struct {
		name  string
		input map[string]any
		want  string
	}{
		{"Bash", map[string]any{"command": "go test ./...\necho done"}, "[bash] go test ./..."},
		{"Edit", map[string]any{"file_path": dir + "/internal/db/db.go"}, "[edit] internal/db/db.go"},
		{"Read", map[string]any{"file_path": "/elsewhere/x.go"}, "[read] /elsewhere/x.go"},
		{"Grep", map[string]any{"pattern": "func New"}, "[grep] func New"},
		{"TodoWrite", map[string]any{}, "[todowrite] update the to-do list"},
		{"SomeNewTool", map[string]any{"x": 1}, "[somenewtool]"},
		{"Bash", map[string]any{"command": strings.Repeat("a", 200)}, "[bash] " + strings.Repeat("a", 117) + "..."},
	}
	for _, tt := range tests {
		if got := describeTool(tt.name, tt.input, dir); got != tt.want {
			t.Errorf("describeTool(%s, %v) = %q, want %q", tt.name, tt.input, got, tt.want)
		}
	}
}

func TestFollowStream(t *testing.T) {
	stream := `{"type":"system","subtype":"init"}
not json at all
{"type":"assistant","message":{"content":[{"type":"text","text":"  \n Looking around.\nmore"},{"type":"tool_use","name":"Read","input":{"file_path":"/r/a.go"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","content":"..."}]}}
{"type":"result","is_error":false,"result":"Done.","num_turns":4,"total_cost_usd":0.25}
`
	var raw, feed strings.Builder
	prog := &progress{}
	res := followStream(strings.NewReader(stream), &raw, &feed, prog, "/r")
	if raw.String() != stream {
		t.Errorf("raw copy differs:\n%s", raw.String())
	}
	if want := "not json at all\n  [claude] Looking around.\n  [read] a.go\n"; feed.String() != want {
		t.Errorf("feed = %q, want %q", feed.String(), want)
	}
	if want := (phaseResult{got: true, text: "Done.", turns: 4, cost: 0.25}); res != want {
		t.Errorf("result = %+v, want %+v", res, want)
	}
	if tools, last := prog.snapshot(); tools != 1 || last != "[read] a.go" {
		t.Errorf("progress = %d, %q", tools, last)
	}
}

func TestShortDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		1400 * time.Millisecond:                    "1s",
		4*time.Minute + 12*time.Second:             "4m12s",
		time.Hour + 3*time.Minute + 20*time.Second: "1h3m",
	} {
		if got := shortDuration(d); got != want {
			t.Errorf("shortDuration(%v) = %q, want %q", d, got, want)
		}
	}
}
