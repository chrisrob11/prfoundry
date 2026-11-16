package main

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"time"
)

// builtinRoles are installed into home/roles by `prf config init`.
//
//go:embed roles/*.md
var builtinRoles embed.FS

// itemName is what a role, pipeline, or repo may be called. Role names
// appear in phase=<name> status lines and repo names are dirs in home.
var itemName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// phase is one Claude run, or a gate. A nil tmpl means the task alone is
// the prompt. A gate's tmpl is its fix role, run once per round of requested
// changes or answers.
type phase struct {
	name      string
	model     string  // from the pipeline step; "" means the default
	maxCost   float64 // from the pipeline step; 0 means the default
	expect    string  // from the pipeline step; "" means the default
	tmpl      *template.Template
	gate      string        // gateReview, gateAsk or gateCheck; "" for a role
	fixRole   string        // the gate's fix role
	maxRounds int           // the gate's limit on rounds
	question  string        // the ask gate's question file
	asker     *phase        // the step an ask gate re-runs: the one before it
	run       string        // the check gate's command; "" means the repo's
	timeout   time.Duration // the check gate's command's limit; 0 means none
	asks      string        // the question file of the ask gate after this role
}

// promptData is what role templates can reference. Context is not for
// templates: render lists those files in every prompt. Review is set only
// for a gate's fix role: the comments to address, or the answers to record.
// Question is set only for an ask gate's: the question they answer.
// Answers is not for templates either: when an ask gate re-runs the step
// that asked, render adds the questions it asked and your answers.
// PriorRun is set only for a role step that ran before in this job: the
// commit it ended at. PriorReport is that run's final message, saved in a
// file, if it left one. render adds a note about both.
type promptData struct {
	PR, Branch, Task, Review, Question string
	Context                            []string
	Answers                            string
	PriorRun, PriorReport              string
}

// roleDirs returns where roles are looked up, in order: the repo's own
// (home/<repo>/roles), then the shared ones (home/roles). With no repo,
// only the shared ones.
func roleDirs(repo string) []string {
	shared := filepath.Join(home, "roles")
	if repo == "" {
		return []string{shared}
	}
	return []string{filepath.Join(home, repo, "roles"), shared}
}

// loadPhases turns pipeline steps into phases. hasWork reports whether a PR
// or task was given; hasTask whether a task was. An ask gate's asker is the
// phase before it.
func loadPhases(steps []step, dirs []string, hasWork, hasTask bool) ([]phase, error) {
	if len(steps) == 0 {
		if !hasTask {
			return nil, errors.New("give --prompt, --pipeline, --role, or a mix")
		}
		return []phase{{name: "prompt"}}, nil
	}

	if err := checkAskers(steps); err != nil {
		return nil, err
	}
	var phases []phase
	for _, s := range steps {
		if s.Role == "implement" && !hasWork {
			return nil, errors.New("role implement needs --pr or --prompt")
		}
		if err := checkGate(s); err != nil {
			return nil, err
		}
		role := s.Role
		if s.Gate != "" {
			role = s.fixRole()
		}
		text, err := findRole(dirs, role)
		if err != nil {
			return nil, err
		}
		p, err := parseRole(role, text)
		if err != nil {
			return nil, err
		}
		p.model, p.maxCost, p.expect = s.Model, s.MaxCost, s.Expect
		if s.Gate != "" {
			p.name, p.gate, p.fixRole = s.name(), s.Gate, role
			p.maxRounds = s.MaxRounds
			if p.maxRounds == 0 {
				p.maxRounds = defaultMaxRounds
			}
		}
		p.run = s.Run
		if s.Timeout != "" {
			p.timeout, _ = time.ParseDuration(s.Timeout) // checkGate parsed it
		}
		if s.Gate == gateAsk {
			p.question = firstNonEmpty(s.Question, defaultQuestion)
			phases[len(phases)-1].asks = p.question
			asker := phases[len(phases)-1]
			p.asker = &asker
		}
		phases = append(phases, p)
	}
	return phases, nil
}

// findRole returns the text of <name>.md from the first of dirs that has it.
func findRole(dirs []string, name string) (string, error) {
	if !itemName.MatchString(name) {
		return "", fmt.Errorf("invalid role name %q: use letters, digits, '-', '_' and '.'", name)
	}
	for _, dir := range dirs {
		b, err := os.ReadFile(filepath.Join(dir, name+".md"))
		if err == nil {
			return string(b), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
	}
	return "", fmt.Errorf("no role %q in %s (prf config init installs the built-in roles)", name, strings.Join(dirs, " or "))
}

func parseRole(name, text string) (phase, error) {
	tmpl, err := template.New(name).Option("missingkey=error").Parse(text)
	if err != nil {
		return phase{}, fmt.Errorf("role %s: %w", name, err)
	}
	return phase{name: name, tmpl: tmpl}, nil
}

// validateRole checks that text parses and renders to a non-empty prompt
// with every mix of PR and task, so a saved role can't break a run later.
func validateRole(name, text string) error {
	p, err := parseRole(name, text)
	if err != nil {
		return err
	}
	for _, data := range []promptData{
		{Branch: "b"},
		{Branch: "b", Review: "## Review"},
		{Branch: "b", Review: "## Answers", Question: "## Question"},
		{Branch: "b", PR: "/spec.md"},
		{Branch: "b", Task: "task"},
		{Branch: "b", PR: "/spec.md", Task: "task"},
		{Branch: "b", PriorRun: "abc1234", PriorReport: "/report.md"},
	} {
		if _, err := p.render(data); err != nil {
			return fmt.Errorf("role %s with PR=%q and task=%q: %w", name, data.PR, data.Task, err)
		}
	}
	return nil
}

// render returns the prompt for one phase: the role's text (or, with no
// role, the task), the codebase context files, and the task. It fails if the
// role's own text is empty, whatever gets added to it.
func (p phase) render(data promptData) (string, error) {
	var b strings.Builder
	if p.tmpl == nil {
		b.WriteString(data.Task)
	} else if err := p.tmpl.Execute(&b, data); err != nil {
		return "", err
	}
	if strings.TrimSpace(b.String()) == "" {
		return "", errors.New("prompt is empty")
	}
	if len(data.Context) > 0 {
		b.WriteString("\n\n## Codebase context\n\nBefore this run, prf prepared these files about the codebase. " +
			"Read index.md first instead of exploring from scratch; it lists every package and says which file " +
			"under packages/ has that package's types and function signatures. To find where a function or type " +
			"is defined, grep symbols.json: one [file, line, declaration] row per symbol, with the file as a number " +
			"you look up in its \"files\" map. " +
			"Use the rest as needed:\n\n")
		for _, f := range data.Context {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}
	if p.tmpl != nil && data.Task != "" {
		fmt.Fprintf(&b, "\n\n## Task\n\n%s\n", data.Task)
	}
	if data.PriorRun != "" {
		report := ""
		if data.PriorReport != "" {
			report = fmt.Sprintf(" and its earlier report (%s)", data.PriorReport)
		}
		fmt.Fprintf(&b, "\n\n## This step ran before\n\nThis step ran before, ending at %s: read `git diff %s..HEAD`%s first, "+
			"and read unchanged code only where you need it.\n", data.PriorRun, data.PriorRun, report)
	}
	if data.Answers != "" {
		fmt.Fprintf(&b, "\n\n## Your questions, answered\n\nAn earlier run of this step stopped to ask the owner. "+
			"Here is what it asked and what the owner answered. Follow the answers, and don't ask these again.\n\n%s", data.Answers)
	}
	return b.String(), nil
}
