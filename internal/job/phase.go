package job

import (
	"cmp"
	"errors"
	"text/template"
	"time"

	"github.com/chrisrob11/prfoundry/internal/agent"
	"github.com/chrisrob11/prfoundry/internal/pipeline"
	"github.com/chrisrob11/prfoundry/internal/roles"
)

// phase is one Claude run, or a gate. A nil tmpl means the task alone is
// the prompt. A gate's tmpl is its fix role, run once per round of requested
// changes or answers.
type phase struct {
	name      string
	model     string  // from the pipeline step; "" means the default
	maxCost   float64 // from the pipeline step; 0 means the default
	expect    string  // from the pipeline step; "" means the default
	tmpl      *template.Template
	gate      string        // pipeline.GateReview, GateAsk or GateCheck; "" for a role
	fixRole   string        // the gate's fix role
	maxRounds int           // the gate's limit on rounds
	question  string        // the ask gate's question file
	asker     *phase        // the step an ask gate re-runs: the one before it
	run       string        // the check gate's command; "" means the repo's
	timeout   time.Duration // the check gate's command's limit; 0 means none
	asks      string        // the question file of the ask gate after this role
}

// loadPhases turns pipeline steps into phases. hasWork reports whether a PR
// or task was given; hasTask whether a task was. An ask gate's asker is the
// phase before it.
func loadPhases(steps []pipeline.Step, dirs []string, hasWork, hasTask bool) ([]phase, error) {
	if len(steps) == 0 {
		if !hasTask {
			return nil, errors.New("give --prompt, --pipeline, --role, or a mix")
		}
		return []phase{{name: "prompt"}}, nil
	}

	if err := pipeline.CheckAskers(steps); err != nil {
		return nil, err
	}
	var phases []phase
	for _, s := range steps {
		if s.Role == "implement" && !hasWork {
			return nil, errors.New("role implement needs --pr or --prompt")
		}
		if err := pipeline.CheckGate(s); err != nil {
			return nil, err
		}
		role := s.Role
		if s.Gate != "" {
			role = s.Fixer()
		}
		text, err := roles.Find(dirs, role)
		if err != nil {
			return nil, err
		}
		p, err := parseRole(role, text)
		if err != nil {
			return nil, err
		}
		p.model, p.maxCost, p.expect = s.Model, s.MaxCost, s.Expect
		if s.Gate != "" {
			p.name, p.gate, p.fixRole = s.Name(), s.Gate, role
			p.maxRounds = s.MaxRounds
			if p.maxRounds == 0 {
				p.maxRounds = pipeline.DefaultMaxRounds
			}
		}
		p.run = s.Run
		if s.Timeout != "" {
			p.timeout, _ = time.ParseDuration(s.Timeout) // pipeline.CheckGate parsed it
		}
		if s.Gate == pipeline.GateAsk {
			p.question = cmp.Or(s.Question, pipeline.DefaultQuestion)
			phases[len(phases)-1].asks = p.question
			asker := phases[len(phases)-1]
			p.asker = &asker
		}
		phases = append(phases, p)
	}
	return phases, nil
}

// stepFor is s as it runs on agentName: on opencode, with its opencode_model
// as its model.
func stepFor(s pipeline.Step, agentName string) pipeline.Step {
	if agentName == agent.OpenCode {
		s.Model = s.OpenCodeModel
	}
	return s
}

// parseRole makes a phase of the role name with text.
func parseRole(name, text string) (phase, error) {
	tmpl, err := roles.Parse(name, text)
	if err != nil {
		return phase{}, err
	}
	return phase{name: name, tmpl: tmpl}, nil
}

// render returns the prompt for p; see roles.Render.
func (p phase) render(data roles.Data) (string, error) {
	return roles.Render(p.tmpl, data)
}
