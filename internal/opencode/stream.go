package opencode

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/chrisrob11/prfoundry/internal/agent"
	"github.com/chrisrob11/prfoundry/internal/human"
)

// event is the part of an `opencode run --format json` line that prf reads.
type event struct {
	Type string `json:"type"` // step_start, text, reasoning, tool_use, step_finish or error
	Part struct {
		Text  string `json:"text"`
		Tool  string `json:"tool"`
		State struct {
			Status string         `json:"status"` // completed or error
			Input  map[string]any `json:"input"`
			Error  string         `json:"error"`
		} `json:"state"`
		Cost   float64 `json:"cost"` // a step's, in USD
		Tokens struct {
			Cache struct {
				Read int64 `json:"read"`
			} `json:"cache"`
		} `json:"tokens"`
	} `json:"part"`
	Error struct {
		Type         string `json:"type"` // e.g. provider.rate-limit
		Message      string `json:"message"`
		RetryAfterMs int64  `json:"retryAfterMs"`
	} `json:"error"`
}

// limitFallback is how long to pause at a usage limit that names no time.
const limitFallback = 30 * time.Minute

// limitErrors are the errors that stop a run until a limit resets. Others
// about limits, like provider.free-limit, say to switch models: waiting
// wouldn't help.
var limitErrors = map[string]bool{
	"provider.rate-limit":   true,
	"provider.budget-limit": true, // "Model use is paused until the limit resets."
}

// follow reads opencode's JSON events until they end. It copies every line
// to out.Raw, writes a readable line for each step to out.Feed (with paths
// under out.Dir shown relative to it), and records tool calls. Each step
// started is a turn, and the run's text is the last step's that had any.
// opencode has no budget of its own, so once the steps' costs reach
// out.Budget, it calls out.Stop and the result is over budget. Lines that
// aren't JSON go to out.Feed as they are.
func follow(r io.Reader, out agent.Output) agent.Result {
	var res agent.Result
	newStep := false
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 64<<20) // tool output can be large
	for sc.Scan() {
		line := sc.Bytes()
		_, _ = fmt.Fprintf(out.Raw, "%s\n", line)
		var ev event
		if json.Unmarshal(line, &ev) != nil {
			_, _ = fmt.Fprintf(out.Feed, "%s\n", line)
			continue
		}
		switch ev.Type {
		case "step_start":
			res.Got, newStep = true, true
			res.Turns++
		case "text":
			if newStep {
				res.Text, newStep = "", false
			}
			res.Text = strings.TrimSpace(res.Text + "\n" + ev.Part.Text)
			if text := human.OneLine(ev.Part.Text, 200); text != "" {
				_, _ = fmt.Fprintf(out.Feed, "  [opencode] %s\n", text)
			}
		case "tool_use":
			activity := describeTool(ev.Part.Tool, ev.Part.State.Input, out.Dir)
			out.Progress.Record(activity)
			if ev.Part.State.Status == "error" {
				activity += " (failed: " + human.OneLine(ev.Part.State.Error, 120) + ")"
			}
			_, _ = fmt.Fprintf(out.Feed, "  %s\n", activity)
		case "step_finish":
			res.Cost += ev.Part.Cost
			res.CacheRead += ev.Part.Tokens.Cache.Read
			if out.Budget > 0 && res.Cost >= out.Budget && !res.OverBudget {
				res.OverBudget = true
				out.Stop()
			}
		case "error":
			res.Got, res.IsError, res.Text = true, true, ev.Error.Message
			_, _ = fmt.Fprintf(out.Feed, "  [opencode] error (%s): %s\n", ev.Error.Type, human.OneLine(ev.Error.Message, 200))
			if limitErrors[ev.Error.Type] {
				wait := limitFallback
				if ev.Error.RetryAfterMs > 0 {
					wait = time.Duration(ev.Error.RetryAfterMs) * time.Millisecond
				}
				res.Limited, res.Reset = true, time.Now().Add(wait)
			}
		}
	}
	return res
}

// describeTool summarizes a tool call as "[tool] what it's working on",
// with paths under dir shown relative to it.
func describeTool(name string, input map[string]any, dir string) string {
	what := ""
	for _, k := range []string{"command", "path", "filePath", "pattern", "url", "query", "description"} {
		if v, ok := input[k].(string); ok && v != "" {
			if dir != "" {
				v = strings.ReplaceAll(v, dir+string(filepath.Separator), "")
			}
			what = human.OneLine(v, 120)
			break
		}
	}
	return strings.TrimSpace(fmt.Sprintf("[%s] %s", strings.ToLower(name), what))
}
