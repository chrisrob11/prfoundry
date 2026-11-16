package claude

import (
	"io"
	"strconv"
	"time"

	"github.com/chrisrob11/prfoundry/internal/agent"
)

// Agent runs Claude Code headless: `claude -p`, reading its stream-json.
type Agent struct {
	bin string
}

// New is Claude Code run from bin.
func New(bin string) Agent {
	return Agent{bin: bin}
}

// Name is "claude".
func (Agent) Name() string { return agent.Claude }

// Bin is the claude binary.
func (a Agent) Bin() string { return a.bin }

// Command is claude's arguments for c, asking for stream-json. Claude runs
// in the checkout, so dir isn't one of them.
func (Agent) Command(c agent.Call, _ string) (agent.Command, error) {
	return agent.Command{Args: args(c)}, nil
}

// Follow reads Claude's stream-json; see follow. Claude holds to its own
// budget, so out.Budget and out.Stop aren't used.
func (Agent) Follow(r io.Reader, out agent.Output) agent.Result {
	res := follow(r, out.Raw, out.Feed, out.Progress, out.Dir)
	res.Reset, res.Limited = limitReset(res, time.Now())
	return res
}

// args are claude's arguments for c, asking for stream-json.
func args(c agent.Call) []string {
	args := []string{"-p", c.Prompt, "--permission-mode", c.Mode, "--output-format", "stream-json", "--verbose"}
	if c.Model != "" {
		args = append(args, "--model", c.Model) // before Extra, so an explicit --model there still wins
	}
	if c.Budget > 0 {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(c.Budget, 'f', -1, 64))
	}
	for _, d := range c.Dirs {
		args = append(args, "--add-dir", d)
	}
	if c.Settings != "" {
		args = append(args, "--settings", c.Settings)
	}
	return append(args, c.Extra...)
}
