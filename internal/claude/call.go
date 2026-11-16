package claude

import "strconv"

// Call is one headless `claude -p` run.
type Call struct {
	Prompt   string
	Mode     string   // --permission-mode
	Model    string   // "" for Claude's default
	Budget   float64  // --max-budget-usd; 0 for none
	Dirs     []string // --add-dir, in order
	Settings string   // --settings; "" for none
	Extra    []string // claude_args and the command line's, last so they win
}

// Args are claude's arguments for the call, asking for stream-json.
func (c Call) Args() []string {
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
