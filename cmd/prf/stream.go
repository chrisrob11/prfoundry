package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// claudeEvent is the part of a `claude -p --output-format stream-json` line
// that prf reads.
type claudeEvent struct {
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

// phaseResult is what Claude's final "result" event said, if it sent one.
type phaseResult struct {
	got     bool
	subtype string // "success", or why it stopped, e.g. "error_max_budget_usd"
	text    string
	isError bool
	turns   int
	cost    float64
	// cacheRead is the context tokens Claude re-read over the run's turns:
	// most of a long step's cost.
	cacheRead int64
}

// progress tracks what Claude is doing during one phase, for the
// periodic "working" line.
type progress struct {
	mu    sync.Mutex
	tools int
	last  string
}

func (p *progress) record(activity string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tools++
	p.last = activity
}

func (p *progress) snapshot() (tools int, last string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tools, p.last
}

// followStream reads Claude's stream-json output until it ends. It copies
// every line to raw, writes a readable line for each step to feed (with
// paths under dir shown relative to it), records tool calls in prog, and
// returns the final result. Lines that aren't JSON (such as errors) go to
// feed as they are.
func followStream(r io.Reader, raw, feed io.Writer, prog *progress, dir string) phaseResult {
	var res phaseResult
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 64<<20) // tool results can be large
	for sc.Scan() {
		line := sc.Bytes()
		fmt.Fprintf(raw, "%s\n", line)
		var ev claudeEvent
		if json.Unmarshal(line, &ev) != nil {
			fmt.Fprintf(feed, "%s\n", line)
			continue
		}
		switch ev.Type {
		case "assistant":
			for _, c := range ev.Message.Content {
				switch c.Type {
				case "text":
					if text := oneLine(c.Text, 200); text != "" {
						fmt.Fprintf(feed, "  [claude] %s\n", text)
					}
				case "tool_use":
					activity := describeTool(c.Name, c.Input, dir)
					prog.record(activity)
					fmt.Fprintf(feed, "  %s\n", activity)
				}
			}
		case "result":
			res = phaseResult{got: true, subtype: ev.Subtype, text: ev.Result, isError: ev.IsError, turns: ev.NumTurns, cost: ev.CostUSD, cacheRead: ev.Usage.CacheRead}
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
				return oneLine(v, 120)
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

// oneLine returns s's first non-empty line, shortened to max characters.
func oneLine(s string, max int) string {
	for line := range strings.SplitSeq(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			if len(line) > max {
				line = line[:max-3] + "..."
			}
			return line
		}
	}
	return ""
}

// shortDuration formats d as 45s, 4m12s, or 1h3m.
func shortDuration(d time.Duration) string {
	if d < time.Hour {
		return d.Round(time.Second).String()
	}
	return strings.TrimSuffix(d.Round(time.Minute).String(), "0s")
}
