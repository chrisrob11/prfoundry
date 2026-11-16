// Package claude is how prf talks to headless Claude Code: the arguments of
// a `claude -p` call, reading the stream-json it prints, and recognizing
// its usage-limit error.
package claude

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"

	"github.com/chrisrob11/prfoundry/internal/human"
)

// event is the part of a `claude -p --output-format stream-json` line
// that prf reads.
type event struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Message struct {
		Content []struct {
			Type  string         `json:"type"`
			Text  string         `json:"text"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
	} `json:"message"`
	Result   string  `json:"result"`
	IsError  bool    `json:"is_error"`
	NumTurns int     `json:"num_turns"`
	CostUSD  float64 `json:"total_cost_usd"`
	Usage    struct {
		CacheRead int64 `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

// Result is what Claude's final "result" event said, if it sent one.
type Result struct {
	Got     bool
	Subtype string // "success", or why it stopped, e.g. "error_max_budget_usd"
	Text    string
	IsError bool
	Turns   int
	Cost    float64
	// CacheRead is the context tokens Claude re-read over the run's turns:
	// most of a long step's cost.
	CacheRead int64
}

// OverBudget reports whether Claude stopped at its --max-budget-usd.
func (r Result) OverBudget() bool {
	return r.Subtype == "error_max_budget_usd"
}

// Progress tracks what Claude is doing during one run, for the periodic
// "working" line.
type Progress struct {
	mu    sync.Mutex
	tools int
	last  string
}

func (p *Progress) record(activity string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tools++
	p.last = activity
}

// Snapshot returns how many tool calls Claude has made, and the last one.
func (p *Progress) Snapshot() (tools int, last string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tools, p.last
}

// Follow reads Claude's stream-json output until it ends. It copies every
// line to raw, writes a readable line for each step to feed (with paths
// under dir shown relative to it), records tool calls in prog, and returns
// the final result. Lines that aren't JSON (such as errors) go to feed as
// they are.
func Follow(r io.Reader, raw, feed io.Writer, prog *Progress, dir string) Result {
	var res Result
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 64<<20) // tool results can be large
	for sc.Scan() {
		line := sc.Bytes()
		_, _ = fmt.Fprintf(raw, "%s\n", line)
		var ev event
		if json.Unmarshal(line, &ev) != nil {
			_, _ = fmt.Fprintf(feed, "%s\n", line)
			continue
		}
		switch ev.Type {
		case "assistant":
			for _, c := range ev.Message.Content {
				switch c.Type {
				case "text":
					if text := human.OneLine(c.Text, 200); text != "" {
						_, _ = fmt.Fprintf(feed, "  [claude] %s\n", text)
					}
				case "tool_use":
					activity := describeTool(c.Name, c.Input, dir)
					prog.record(activity)
					_, _ = fmt.Fprintf(feed, "  %s\n", activity)
				}
			}
		case "result":
			res = Result{Got: true, Subtype: ev.Subtype, Text: ev.Result, IsError: ev.IsError, Turns: ev.NumTurns, Cost: ev.CostUSD, CacheRead: ev.Usage.CacheRead}
		}
	}
	return res
}

// describeTool summarizes a tool call as "[tool] what it's working on",
// with paths under dir shown relative to it.
func describeTool(name string, input map[string]any, dir string) string {
	arg := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := input[k].(string); ok && v != "" {
				if dir != "" {
					v = strings.ReplaceAll(v, dir+string(filepath.Separator), "")
				}
				return human.OneLine(v, 120)
			}
		}
		return ""
	}
	var what string
	switch name {
	case "Bash":
		what = arg("command")
	case "Read", "Edit", "Write", "MultiEdit", "NotebookEdit":
		what = arg("file_path", "notebook_path")
	case "Grep", "Glob":
		what = arg("pattern")
	case "WebFetch":
		what = arg("url")
	case "WebSearch":
		what = arg("query")
	case "Task", "Agent":
		what = arg("description")
	case "TodoWrite":
		what = "update the to-do list"
	}
	return strings.TrimSpace(fmt.Sprintf("[%s] %s", strings.ToLower(name), what))
}
