// Package opencode runs opencode headless for prf: the arguments and
// permission config of an `opencode run --format json` call, and reading
// the JSON events it prints, its usage limits among them.
package opencode

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"

	"github.com/chrisrob11/prfoundry/internal/agent"
)

// Agent runs opencode headless: `opencode run --format json`.
type Agent struct {
	bin string
}

// New is opencode run from bin.
func New(bin string) Agent {
	return Agent{bin: bin}
}

// Name is "opencode".
func (Agent) Name() string { return agent.OpenCode }

// Bin is the opencode binary.
func (a Agent) Bin() string { return a.bin }

// modelName is an opencode model: provider/model, with an optional #variant.
var modelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.\-]*/\S+$`)

// ValidModel is whether name is an opencode model, provider/model.
func ValidModel(name string) bool { return modelName.MatchString(name) }

// errSandbox is why a sandboxed job can't run opencode.
var errSandbox = errors.New("opencode has no sandbox like Claude Code's, so it can't honour --sandbox; " +
	"run it unsandboxed with --sandbox=false (or \"sandbox\": false in conf.json)")

// Check is Command's refusal of a mode, a model or the sandbox, before a
// job starts.
func Check(mode, model string, sandboxed bool) error {
	if sandboxed {
		return errSandbox
	}
	if model != "" && !ValidModel(model) {
		return fmt.Errorf("opencode model %q must be provider/model, e.g. anthropic/claude-sonnet-5-5", model)
	}
	_, _, err := modeRules(mode)
	return err
}

// Command is opencode's arguments for c in the checkout dir, and its
// environment: PWD, which opencode takes as its project folder over its
// working directory, and the permission config for c's mode. It runs a
// private server (--standalone), so that environment reaches its tools.
func (Agent) Command(c agent.Call, dir string) (agent.Command, error) {
	if err := Check(c.Mode, c.Model, c.Settings != ""); err != nil {
		return agent.Command{}, err
	}
	rules, auto, _ := modeRules(c.Mode)
	conf, err := json.Marshal(map[string]any{"permissions": permissions(rules, c.Dirs)})
	if err != nil {
		return agent.Command{}, err
	}
	args := []string{"run", "--standalone", "--format", "json"}
	if c.Model != "" {
		args = append(args, "--model", c.Model)
	}
	if auto {
		args = append(args, "--auto")
	}
	args = append(append(args, c.Extra...), c.Prompt)
	return agent.Command{Args: args, Env: []string{"PWD=" + dir, "OPENCODE_CONFIG_CONTENT=" + string(conf)}}, nil
}

// Follow reads opencode's JSON events; see follow.
func (Agent) Follow(r io.Reader, out agent.Output) agent.Result {
	return follow(r, out)
}
