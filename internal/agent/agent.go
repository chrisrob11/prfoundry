// Package agent is what a job needs from the coding agent it runs headless:
// the command line of one run, and what the run's output says. Claude Code
// (internal/claude) and opencode (internal/opencode) implement it.
package agent

import (
	"io"
	"sync"
	"time"
)

// Agent is a coding agent prf runs headless, one run per phase.
type Agent interface {
	// Name is the agent as state.json and prf's lines name it.
	Name() string
	// Bin is the agent's binary.
	Bin() string
	// Command is how to start a run of c in the checkout dir. Its error is
	// a usage error: a setting the agent can't honour.
	Command(c Call, dir string) (Command, error)
	// Follow reads a run's output until it ends, and returns its result.
	Follow(r io.Reader, out Output) Result
}

// Call is one headless run.
type Call struct {
	Prompt   string
	Mode     string   // prf's --mode, in Claude Code's permission-mode names
	Model    string   // "" for the agent's default
	Budget   float64  // the run's budget in USD; 0 for none
	Dirs     []string // folders outside the checkout the run may use
	Settings string   // the Claude Code settings file that turns its sandbox on; "" when off
	Extra    []string // conf.json's arguments for the agent and the command line's, last so they win
}

// Command starts a run: the agent's arguments, and what to add to the
// job's environment.
type Command struct {
	Args []string
	Env  []string
}

// Output is where Follow sends what a run prints.
type Output struct {
	Raw      io.Writer // every line, as printed
	Feed     io.Writer // a readable line for each step
	Progress *Progress
	Dir      string  // the checkout: paths under it are shown relative to it
	Budget   float64 // the run's budget, for an agent that can't hold to one itself; 0 for none
	Stop     func()  // asks the run to stop, once it has spent Budget
}

// Result is what a run reported.
type Result struct {
	Got        bool // the run reported a result
	Text       string
	IsError    bool
	OverBudget bool // it stopped at its budget
	Turns      int
	Cost       float64
	// CacheRead is the context tokens re-read over the run's turns: most
	// of a long step's cost.
	CacheRead int64
	Limited   bool      // it stopped at a usage limit
	Reset     time.Time // when that limit resets
}

// Progress tracks what a run is doing, for the periodic "working" line.
type Progress struct {
	mu    sync.Mutex
	tools int
	last  string
}

// Record counts a tool call, described as activity.
func (p *Progress) Record(activity string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tools++
	p.last = activity
}

// Snapshot returns how many tool calls the run has made, and the last one.
func (p *Progress) Snapshot() (tools int, last string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tools, p.last
}
