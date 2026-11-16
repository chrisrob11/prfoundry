// Package pipeline owns a repo's pipelines: the steps (roles and gates)
// they run, how steps are written on the command line and in
// pipeline.json, their validation, and reading and writing that file.
package pipeline

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/chrisrob11/prfoundry/internal/atomicfile"
	"github.com/chrisrob11/prfoundry/internal/jsonio"
	"github.com/chrisrob11/prfoundry/internal/opencode"
)

// FileName is the file that holds a repo's pipelines: <home>/<repo>/pipeline.json.
const FileName = "pipeline.json"

// Builtin is the "default" pipeline of a repo with no pipeline.json.
func Builtin() []Step {
	return []Step{{Role: "implement"}, {Role: "test"}, {Role: "review"}}
}

// File is the content of a pipeline.json: named role sequences, which
// one runs when --pipeline isn't given, the model and expectation steps use
// unless they name their own, and how to prepare a fresh checkout.
type File struct {
	Default       string            `json:"default,omitempty"`
	Model         string            `json:"model,omitempty"`
	OpenCodeModel string            `json:"opencode_model,omitempty"` // Model, when the agent is opencode
	MaxCost       float64           `json:"max_cost,omitempty"`       // the whole job's budget in USD
	MaxStepCost   float64           `json:"max_step_cost,omitempty"`  // each step's budget, unless it sets max_cost
	Pipelines     map[string][]Step `json:"pipelines"`
	Index         *bool             `json:"index,omitempty"`  // build the codebase map; default true
	Setup         []string          `json:"setup,omitempty"`  // shell commands run in the checkout before the first phase
	Check         string            `json:"check,omitempty"`  // the command @check runs, unless it names its own
	Expect        string            `json:"expect,omitempty"` // what role steps must leave behind, unless they set their own
}

// Step is one stage of a pipeline: a role, and the model it runs on if it
// names one, or a gate. In pipeline.json it's "role" or {"role": ..., "model": ...};
// on the command line, "role" or "role:model". A gate is "@review", "@ask"
// or "@check" in either, or {"gate": "review", ...} with its options;
// its model is the one its fix role runs on.
type Step struct {
	Role          string  `json:"role,omitempty"`
	Gate          string  `json:"gate,omitempty"`       // review, ask or check: a gate instead of a role
	FixRole       string  `json:"fix_role,omitempty"`   // the gate's role for acting on what you said, or on what failed
	MaxRounds     int     `json:"max_rounds,omitempty"` // the gate's limit on rounds
	Question      string  `json:"question,omitempty"`   // the ask gate's question file
	Run           string  `json:"run,omitempty"`        // the check gate's command
	Timeout       string  `json:"timeout,omitempty"`    // how long the check gate's command may take
	Model         string  `json:"model,omitempty"`
	OpenCodeModel string  `json:"opencode_model,omitempty"` // Model, when the agent is opencode
	MaxCost       float64 `json:"max_cost,omitempty"`       // this step's budget in USD
	Expect        string  `json:"expect,omitempty"`         // what the role must leave behind: commits, read-only or clean
}

// The gates and their defaults.
const (
	GateReview       = "review"
	GateAsk          = "ask"
	GateCheck        = "check"
	DefaultQuestion  = "$PRF_TMP/stop.md"
	DefaultMaxRounds = 5
)

// What a role step can be expected to leave behind. Each also means no
// uncommitted changes.
const (
	ExpectCommits  = "commits"   // at least one new commit
	ExpectReadOnly = "read-only" // no new commits
	ExpectClean    = "clean"     // nothing more
	expectList     = `"` + ExpectCommits + `", "` + ExpectReadOnly + `" or "` + ExpectClean + `"`
)

// validExpect is whether v is one of the expect values.
func validExpect(v string) bool {
	return v == ExpectCommits || v == ExpectReadOnly || v == ExpectClean
}

// defaultFixRole is gate's default fix role, or "" for an unknown gate.
func defaultFixRole(gate string) string {
	switch gate {
	case GateReview:
		return "address-review"
	case GateAsk:
		return "answer-question"
	case GateCheck:
		return "fix-check"
	}
	return ""
}

// Fixer is the role a gate runs on what you said, or on what failed: its
// fix_role, or its gate's default.
func (s Step) Fixer() string {
	return cmp.Or(s.FixRole, defaultFixRole(s.Gate))
}

// modelName is what a model may be called: an alias like opus or a full
// name like claude-opus-5-5, optionally with a suffix like [1m].
var modelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.\-\[\]]*$`)

// ValidModel is whether name is a valid model name.
func ValidModel(name string) bool { return modelName.MatchString(name) }

// UnmarshalJSON reads a step as its name ("role" or "@gate") or an object.
func (s *Step) UnmarshalJSON(b []byte) error {
	var role string
	if json.Unmarshal(b, &role) == nil {
		if gate, ok := strings.CutPrefix(role, "@"); ok {
			s.Gate = gate
		} else {
			s.Role = role
		}
		return nil
	}
	type plain Step
	if err := jsonio.DecodeStrict(b, (*plain)(s)); err != nil {
		return fmt.Errorf(`a step is "role", "@review", "@ask", "@check", {"role": "...", "model": "...", "max_cost": ..., "expect": "..."}, {"gate": "review", "fix_role": "...", "max_rounds": ...}, {"gate": "ask", "question": "...", ...} or {"gate": "check", "run": "...", "timeout": "10m", ...}: %w`, err)
	}
	return nil
}

// MarshalJSON writes a step with only a role (or gate) as just its name.
func (s Step) MarshalJSON() ([]byte, error) {
	if s.Model == "" && s.OpenCodeModel == "" && s.MaxCost == 0 && s.FixRole == "" && s.MaxRounds == 0 && s.Question == "" && s.Run == "" && s.Timeout == "" && s.Expect == "" {
		return json.Marshal(s.Name())
	}
	type plain Step
	return json.Marshal(plain(s))
}

// Name is the step's role, or @ and its gate.
func (s Step) Name() string {
	if s.Gate != "" {
		return "@" + s.Gate
	}
	return s.Role
}

// String is the command-line form: role or role:model.
func (s Step) String() string {
	if s.Model == "" {
		return s.Name()
	}
	return s.Name() + ":" + s.Model
}

// ParseStep reads "role", "role:model", "@gate" or "@gate:model".
func ParseStep(text string) (Step, error) {
	role, model, hasModel := strings.Cut(text, ":")
	s := Step{Role: strings.TrimSpace(role), Model: strings.TrimSpace(model)}
	if gate, ok := strings.CutPrefix(s.Role, "@"); ok {
		s.Role, s.Gate = "", gate
	}
	if hasModel && !modelName.MatchString(s.Model) {
		return Step{}, fmt.Errorf("step %q: invalid model %q", text, s.Model)
	}
	return s, nil
}

// Join is steps in their command-line form, separated by sep.
func Join(steps []Step, sep string) string {
	parts := make([]string, len(steps))
	for i, s := range steps {
		parts[i] = s.String()
	}
	return strings.Join(parts, sep)
}

// Path is repo's pipeline.json under prf's home folder.
func Path(home, repo string) string {
	return filepath.Join(home, repo, FileName)
}

// Read returns repo's pipeline.json, under home. A repo without one gets the
// built-in default pipeline, and exists is false.
func Read(home, repo string) (p File, exists bool, err error) {
	path := Path(home, repo)
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return File{Default: "default", Pipelines: map[string][]Step{"default": Builtin()}}, false, nil
	}
	if err == nil {
		err = jsonio.DecodeStrict(b, &p)
	}
	if err != nil {
		return File{}, false, fmt.Errorf("%s: %w", path, err)
	}
	if p.Pipelines == nil {
		p.Pipelines = map[string][]Step{}
	}
	if p.Model != "" && !modelName.MatchString(p.Model) {
		return File{}, false, fmt.Errorf("%s: invalid model %q", path, p.Model)
	}
	if p.OpenCodeModel != "" && !opencode.ValidModel(p.OpenCodeModel) {
		return File{}, false, fmt.Errorf("%s: opencode_model must be provider/model, not %q", path, p.OpenCodeModel)
	}
	if p.Expect != "" && !validExpect(p.Expect) {
		return File{}, false, fmt.Errorf("%s: expect must be %s, not %q", path, expectList, p.Expect)
	}
	return p, true, nil
}

// Write replaces repo's pipeline.json, under home, with p.
func Write(home, repo string, p File) error {
	var b strings.Builder
	if err := jsonio.Write(&b, p); err != nil {
		return err
	}
	return atomicfile.Write(Path(home, repo), []byte(b.String()))
}

// Lookup returns the steps of repo's pipeline name, read from under home, or of its default
// pipeline when name is empty, along with the name it used.
func Lookup(home, repo, name string) ([]Step, string, error) {
	p, _, err := Read(home, repo)
	if err != nil {
		return nil, "", err
	}
	if name == "" {
		if name = p.Default; name == "" {
			return nil, "", fmt.Errorf("%s has no default pipeline; give --pipeline or --role", Path(home, repo))
		}
	}
	roles, ok := p.Pipelines[name]
	if !ok {
		return nil, "", fmt.Errorf("repo %s has no pipeline %q (it has: %s)", repo, name, strings.Join(slices.Sorted(maps.Keys(p.Pipelines)), ", "))
	}
	if len(roles) == 0 {
		return nil, "", fmt.Errorf("pipeline %s of repo %s lists no roles", name, repo)
	}
	return roles, name, nil
}

// CheckGate reports what's wrong with a step's gate settings: an unknown
// gate, a gate that also names a role, gate options on a role, an option
// for another gate, or an expect that's unknown or on a gate.
func CheckGate(s Step) error {
	switch {
	case s.Gate == "":
		if s.FixRole != "" || s.MaxRounds != 0 || s.Question != "" || s.Run != "" || s.Timeout != "" {
			return fmt.Errorf("step %s: fix_role, max_rounds, question, run and timeout are only for gates (%s)", s.Role, gateList)
		}
		if s.Expect != "" && !validExpect(s.Expect) {
			return fmt.Errorf("step %s: expect must be %s, not %q", s.Role, expectList, s.Expect)
		}
	case defaultFixRole(s.Gate) == "":
		return fmt.Errorf("unknown gate @%s; the gates are %s", s.Gate, gateList)
	case s.Role != "":
		return fmt.Errorf("step %s: a step is a role or a gate, not both", s.Name())
	case s.Expect != "":
		return fmt.Errorf("step %s: expect is only for roles; a gate's fix role commits what it fixes", s.Name())
	case s.MaxRounds < 0:
		return fmt.Errorf("step %s: max_rounds can't be negative", s.Name())
	case s.Question != "" && s.Gate != GateAsk:
		return fmt.Errorf("step %s: question is only for the @%s gate", s.Name(), GateAsk)
	case s.Run != "" && s.Gate != GateCheck:
		return fmt.Errorf("step %s: run is only for the @%s gate", s.Name(), GateCheck)
	case s.Timeout != "" && s.Gate != GateCheck:
		return fmt.Errorf("step %s: timeout is only for the @%s gate", s.Name(), GateCheck)
	case s.Timeout != "":
		if d, err := time.ParseDuration(s.Timeout); err != nil || d <= 0 {
			return fmt.Errorf("step %s: timeout %q isn't a duration like 10m or 1h30m", s.Name(), s.Timeout)
		}
	}
	return nil
}

// gateList names the gates for error messages.
const gateList = "@" + GateReview + ", @" + GateAsk + " and @" + GateCheck

// CheckCommands reports a @check gate with no command: neither its own run
// nor the repo's check (def).
func CheckCommands(steps []Step, def string) error {
	for i, s := range steps {
		if s.Gate == GateCheck && strings.TrimSpace(s.Run) == "" && strings.TrimSpace(def) == "" {
			return fmt.Errorf("step %d: @%s has no command: give it \"run\", or set \"check\" in %s", i+1, GateCheck, FileName)
		}
	}
	return nil
}

// CheckAskers reports an @ask gate that doesn't follow a role: the gate
// re-runs the step before it once you answer its question.
func CheckAskers(steps []Step) error {
	for i, s := range steps {
		if s.Gate == GateAsk && (i == 0 || steps[i-1].Gate != "") {
			return fmt.Errorf("step %d: @%s must follow a role, the step whose questions it asks you and re-runs", i+1, GateAsk)
		}
	}
	return nil
}

// ParseSteps reads steps (role or role:model) one per line or
// comma-separated, with '#' starting a comment.
func ParseSteps(text string) ([]Step, error) {
	var steps []Step
	for line := range strings.SplitSeq(text, "\n") {
		line, _, _ = strings.Cut(line, "#")
		for item := range strings.SplitSeq(line, ",") {
			if item = strings.TrimSpace(item); item == "" {
				continue
			}
			s, err := ParseStep(item)
			if err != nil {
				return nil, err
			}
			steps = append(steps, s)
		}
	}
	return steps, nil
}
