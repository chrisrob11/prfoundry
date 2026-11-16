package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/urfave/cli/v3"
)

// pipelineFile holds a repo's pipelines: home/<repo>/pipeline.json.
const pipelineFile = "pipeline.json"

// builtinPipeline is the "default" pipeline of a repo with no pipeline.json.
var builtinPipeline = []step{{Role: "implement"}, {Role: "test"}, {Role: "review"}}

// pipelines is the content of a pipeline.json: named role sequences, which
// one runs when --pipeline isn't given, the model and expectation steps use
// unless they name their own, and how to prepare a fresh checkout.
type pipelines struct {
	Default     string            `json:"default,omitempty"`
	Model       string            `json:"model,omitempty"`
	MaxCost     float64           `json:"max_cost,omitempty"`      // the whole job's budget in USD
	MaxStepCost float64           `json:"max_step_cost,omitempty"` // each step's budget, unless it sets max_cost
	Pipelines   map[string][]step `json:"pipelines"`
	Index       *bool             `json:"index,omitempty"`  // build the codebase map; default true
	Setup       []string          `json:"setup,omitempty"`  // shell commands run in the checkout before the first phase
	Check       string            `json:"check,omitempty"`  // the command @check runs, unless it names its own
	Expect      string            `json:"expect,omitempty"` // what role steps must leave behind, unless they set their own
}

// step is one stage of a pipeline: a role, and the model it runs on if it
// names one, or a gate. In pipeline.json it's "role" or {"role": ..., "model": ...};
// on the command line, "role" or "role:model". A gate is "@review", "@ask"
// or "@check" in either, or {"gate": "review", ...} with its options;
// its model is the one its fix role runs on.
type step struct {
	Role      string  `json:"role,omitempty"`
	Gate      string  `json:"gate,omitempty"`       // review, ask or check: a gate instead of a role
	FixRole   string  `json:"fix_role,omitempty"`   // the gate's role for acting on what you said, or on what failed
	MaxRounds int     `json:"max_rounds,omitempty"` // the gate's limit on rounds
	Question  string  `json:"question,omitempty"`   // the ask gate's question file
	Run       string  `json:"run,omitempty"`        // the check gate's command
	Timeout   string  `json:"timeout,omitempty"`    // how long the check gate's command may take
	Model     string  `json:"model,omitempty"`
	MaxCost   float64 `json:"max_cost,omitempty"` // this step's budget in USD
	Expect    string  `json:"expect,omitempty"`   // what the role must leave behind: commits, read-only or clean
}

// The gates and their defaults.
const (
	gateReview       = "review"
	gateAsk          = "ask"
	gateCheck        = "check"
	defaultQuestion  = "$PRF_TMP/stop.md"
	defaultMaxRounds = 5
)

// gateFixRoles is each gate's default fix role.
// What a role step can be expected to leave behind. Each also means no
// uncommitted changes.
const (
	expectCommits  = "commits"   // at least one new commit
	expectReadOnly = "read-only" // no new commits
	expectClean    = "clean"     // nothing more
	expectList     = `"` + expectCommits + `", "` + expectReadOnly + `" or "` + expectClean + `"`
)

func validExpect(v string) bool {
	return v == expectCommits || v == expectReadOnly || v == expectClean
}

var gateFixRoles = map[string]string{
	gateReview: "address-review",
	gateAsk:    "answer-question",
	gateCheck:  "fix-check",
}

// fixRole is the role a gate runs on what you said, or on what failed: its
// fix_role, or its gate's default.
func (s step) fixRole() string {
	return firstNonEmpty(s.FixRole, gateFixRoles[s.Gate])
}

// modelName is what a model may be called: an alias like opus or a full
// name like claude-opus-5-5, optionally with a suffix like [1m].
var modelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.\-\[\]]*$`)

func (s *step) UnmarshalJSON(b []byte) error {
	var role string
	if json.Unmarshal(b, &role) == nil {
		if gate, ok := strings.CutPrefix(role, "@"); ok {
			s.Gate = gate
		} else {
			s.Role = role
		}
		return nil
	}
	type plain step
	if err := decodeStrict(b, (*plain)(s)); err != nil {
		return fmt.Errorf(`a step is "role", "@review", "@ask", "@check", {"role": "...", "model": "...", "max_cost": ..., "expect": "..."}, {"gate": "review", "fix_role": "...", "max_rounds": ...}, {"gate": "ask", "question": "...", ...} or {"gate": "check", "run": "...", "timeout": "10m", ...}: %w`, err)
	}
	return nil
}

// MarshalJSON writes a step with only a role (or gate) as just its name.
func (s step) MarshalJSON() ([]byte, error) {
	if s.Model == "" && s.MaxCost == 0 && s.FixRole == "" && s.MaxRounds == 0 && s.Question == "" && s.Run == "" && s.Timeout == "" && s.Expect == "" {
		return json.Marshal(s.name())
	}
	type plain step
	return json.Marshal(plain(s))
}

// name is the step's role, or @ and its gate.
func (s step) name() string {
	if s.Gate != "" {
		return "@" + s.Gate
	}
	return s.Role
}

// String is the command-line form: role or role:model.
func (s step) String() string {
	if s.Model == "" {
		return s.name()
	}
	return s.name() + ":" + s.Model
}

// parseStep reads "role", "role:model", "@gate" or "@gate:model".
func parseStep(text string) (step, error) {
	role, model, hasModel := strings.Cut(text, ":")
	s := step{Role: strings.TrimSpace(role), Model: strings.TrimSpace(model)}
	if gate, ok := strings.CutPrefix(s.Role, "@"); ok {
		s.Role, s.Gate = "", gate
	}
	if hasModel && !modelName.MatchString(s.Model) {
		return step{}, fmt.Errorf("step %q: invalid model %q", text, s.Model)
	}
	return s, nil
}

func joinSteps(steps []step, sep string) string {
	parts := make([]string, len(steps))
	for i, s := range steps {
		parts[i] = s.String()
	}
	return strings.Join(parts, sep)
}

func pipelinePath(repo string) string {
	return filepath.Join(home, repo, pipelineFile)
}

// readPipelines returns repo's pipeline.json. A repo without one gets the
// built-in default pipeline, and exists is false.
func readPipelines(repo string) (p pipelines, exists bool, err error) {
	path := pipelinePath(repo)
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return pipelines{Default: "default", Pipelines: map[string][]step{"default": builtinPipeline}}, false, nil
	}
	if err == nil {
		err = decodeStrict(b, &p)
	}
	if err != nil {
		return pipelines{}, false, fmt.Errorf("%s: %w", path, err)
	}
	if p.Pipelines == nil {
		p.Pipelines = map[string][]step{}
	}
	if p.Model != "" && !modelName.MatchString(p.Model) {
		return pipelines{}, false, fmt.Errorf("%s: invalid model %q", path, p.Model)
	}
	if p.Expect != "" && !validExpect(p.Expect) {
		return pipelines{}, false, fmt.Errorf("%s: expect must be %s, not %q", path, expectList, p.Expect)
	}
	return p, true, nil
}

func writePipelines(repo string, p pipelines) error {
	var b strings.Builder
	if err := writeJSON(&b, p); err != nil {
		return err
	}
	return writeAtomic(pipelinePath(repo), []byte(b.String()))
}

// pipelineRoles returns the steps of repo's pipeline name, or of its default
// pipeline when name is empty, along with the name it used.
func pipelineRoles(repo, name string) ([]step, string, error) {
	p, _, err := readPipelines(repo)
	if err != nil {
		return nil, "", err
	}
	if name == "" {
		if name = p.Default; name == "" {
			return nil, "", fmt.Errorf("%s has no default pipeline; give --pipeline or --role", pipelinePath(repo))
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

// checkPipeline reports the first step that names a missing or invalid role,
// or an invalid model, for repo, or a @check with no command to run.
func checkPipeline(repo string, steps []step) error {
	if len(steps) == 0 {
		return errors.New("lists no roles")
	}
	if err := checkAskers(steps); err != nil {
		return err
	}
	p, _, _ := readPipelines(repo) // a broken file is reported by whoever read the steps from it
	if err := checkCommands(steps, p.Check); err != nil {
		return err
	}
	for _, s := range steps {
		if s.Model != "" && !modelName.MatchString(s.Model) {
			return fmt.Errorf("step %s: invalid model %q", s.name(), s.Model)
		}
		if s.MaxCost < 0 {
			return fmt.Errorf("step %s: max_cost can't be negative", s.name())
		}
		if err := checkGate(s); err != nil {
			return err
		}
		role := s.Role
		if s.Gate != "" {
			role = s.fixRole() // the gate needs its fix role
		}
		text, err := findRole(roleDirs(repo), role)
		if err == nil {
			err = validateRole(role, text)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// checkGate reports what's wrong with a step's gate settings: an unknown
// gate, a gate that also names a role, gate options on a role, an option
// for another gate, or an expect that's unknown or on a gate.
func checkGate(s step) error {
	switch {
	case s.Gate == "":
		if s.FixRole != "" || s.MaxRounds != 0 || s.Question != "" || s.Run != "" || s.Timeout != "" {
			return fmt.Errorf("step %s: fix_role, max_rounds, question, run and timeout are only for gates (%s)", s.Role, gateList)
		}
		if s.Expect != "" && !validExpect(s.Expect) {
			return fmt.Errorf("step %s: expect must be %s, not %q", s.Role, expectList, s.Expect)
		}
	case gateFixRoles[s.Gate] == "":
		return fmt.Errorf("unknown gate @%s; the gates are %s", s.Gate, gateList)
	case s.Role != "":
		return fmt.Errorf("step %s: a step is a role or a gate, not both", s.name())
	case s.Expect != "":
		return fmt.Errorf("step %s: expect is only for roles; a gate's fix role commits what it fixes", s.name())
	case s.MaxRounds < 0:
		return fmt.Errorf("step %s: max_rounds can't be negative", s.name())
	case s.Question != "" && s.Gate != gateAsk:
		return fmt.Errorf("step %s: question is only for the @%s gate", s.name(), gateAsk)
	case s.Run != "" && s.Gate != gateCheck:
		return fmt.Errorf("step %s: run is only for the @%s gate", s.name(), gateCheck)
	case s.Timeout != "" && s.Gate != gateCheck:
		return fmt.Errorf("step %s: timeout is only for the @%s gate", s.name(), gateCheck)
	case s.Timeout != "":
		if d, err := time.ParseDuration(s.Timeout); err != nil || d <= 0 {
			return fmt.Errorf("step %s: timeout %q isn't a duration like 10m or 1h30m", s.name(), s.Timeout)
		}
	}
	return nil
}

// gateList names the gates for error messages.
const gateList = "@" + gateReview + ", @" + gateAsk + " and @" + gateCheck

// checkCommands reports a @check gate with no command: neither its own run
// nor the repo's check (def).
func checkCommands(steps []step, def string) error {
	for i, s := range steps {
		if s.Gate == gateCheck && strings.TrimSpace(s.Run) == "" && strings.TrimSpace(def) == "" {
			return fmt.Errorf("step %d: @%s has no command: give it \"run\", or set \"check\" in %s", i+1, gateCheck, pipelineFile)
		}
	}
	return nil
}

// checkAskers reports an @ask gate that doesn't follow a role: the gate
// re-runs the step before it once you answer its question.
func checkAskers(steps []step) error {
	for i, s := range steps {
		if s.Gate == gateAsk && (i == 0 || steps[i-1].Gate != "") {
			return fmt.Errorf("step %d: @%s must follow a role, the step whose questions it asks you and re-runs", i+1, gateAsk)
		}
	}
	return nil
}

// parseRoleList reads steps (role or role:model) one per line or
// comma-separated, with '#' starting a comment.
func parseRoleList(text string) ([]step, error) {
	var steps []step
	for line := range strings.SplitSeq(text, "\n") {
		line, _, _ = strings.Cut(line, "#")
		for item := range strings.SplitSeq(line, ",") {
			if item = strings.TrimSpace(item); item == "" {
				continue
			}
			s, err := parseStep(item)
			if err != nil {
				return nil, err
			}
			steps = append(steps, s)
		}
	}
	return steps, nil
}

// pipelineEntry describes one pipeline as list and show report it.
type pipelineEntry struct {
	Name    string `json:"name"`
	Steps   []step `json:"steps"`
	Default bool   `json:"default"`
	Valid   bool   `json:"valid"`
	Error   string `json:"error,omitempty"`
}

func pipelineCmd() *cli.Command {
	repoFlag := &cli.StringFlag{Name: "repo", Required: true, Usage: "repo whose " + pipelineFile + " to use (an alias or repo name)"}
	jsonFlag := &cli.BoolFlag{Name: "json", Usage: "print JSON"}
	return &cli.Command{
		Name:  "pipeline",
		Usage: "list, show, and change a repo's pipelines; built for LLMs and scripts as well as people",
		Description: "A repo's pipelines live in ~/.prf/<repo>/" + pipelineFile + ": named lists of steps,\n" +
			"run in order, plus the default one. A step is a role, optionally with the model it\n" +
			"runs on: role:model here (e.g. implement:opus), {\"role\": ..., \"model\": ...} in the file.\n" +
			"@review is a human review gate: the job waits for your review in the browser, and its fix role\n" +
			"addresses your comments until you approve. @ask, after a role, shows you the question that role\n" +
			"left ($PRF_TMP/stop.md) beside the diff, records your answers, and re-runs the role.\n" +
			"@check runs a command (its \"run\", or the file's \"check\") and has its fix role fix what fails;\n" +
			"A repo without the file has a built-in \"default\" pipeline: " + joinSteps(builtinPipeline, ", ") + ".\n" +
			"set refuses a pipeline that names a missing or invalid role, and leaves the file unchanged.",
		Commands: []*cli.Command{
			{Name: "list", Usage: "list the repo's pipelines, which is the default, and whether each is valid", Flags: []cli.Flag{repoFlag, jsonFlag}, Action: pipelineList},
			{Name: "show", Usage: "print a pipeline's steps (role or role:model), one per line", ArgsUsage: "<name>", Flags: []cli.Flag{repoFlag, jsonFlag}, Action: pipelineShow},
			{Name: "set", Usage: "create or replace a pipeline from --roles, --file, or stdin (steps one per line or comma-separated)", ArgsUsage: "<name>",
				Flags: []cli.Flag{repoFlag,
					&cli.StringFlag{Name: "roles", Usage: "comma-separated steps (role or role:model), instead of stdin"},
					&cli.StringFlag{Name: "file", Usage: "read roles from this file instead of stdin"},
					&cli.BoolFlag{Name: "default", Usage: "also make it the default pipeline"},
				}, Action: pipelineSet},
			{Name: "remove", Usage: "delete a pipeline", ArgsUsage: "<name>", Flags: []cli.Flag{repoFlag}, Action: pipelineRemove},
			{Name: "default", Usage: "make a pipeline the one that runs when --pipeline isn't given", ArgsUsage: "<name>", Flags: []cli.Flag{repoFlag}, Action: pipelineDefault},
		},
	}
}

func pipelineList(ctx context.Context, c *cli.Command) error {
	repo, err := repoArg(c)
	if err != nil {
		return err
	}
	p, exists, err := readPipelines(repo)
	if err != nil {
		return cli.Exit("prf: "+err.Error(), exitUsage)
	}
	var entries []pipelineEntry
	for _, name := range slices.Sorted(maps.Keys(p.Pipelines)) {
		e := pipelineEntry{Name: name, Steps: p.Pipelines[name], Default: name == p.Default}
		if err := checkPipeline(repo, e.Steps); err != nil {
			e.Error = err.Error()
		}
		e.Valid = e.Error == ""
		entries = append(entries, e)
	}

	w := c.Root().Writer
	if c.Bool("json") {
		return writeJSON(w, entries)
	}
	if !exists {
		_, _ = fmt.Fprintf(w, "# %s doesn't exist; using the built-in default\n", pipelinePath(repo))
	}
	if m := firstNonEmpty(p.Model, cfg.Model); m != "" {
		_, _ = fmt.Fprintf(w, "# steps without a model use %s\n", m)
	}
	for _, e := range entries {
		mark, status := " ", ""
		if e.Default {
			mark = "*"
		}
		if !e.Valid {
			status = "  INVALID: " + e.Error
		}
		_, _ = fmt.Fprintf(w, "%s %-12s %s%s\n", mark, e.Name, joinSteps(e.Steps, " -> "), status)
	}
	return nil
}

func pipelineShow(ctx context.Context, c *cli.Command) error {
	repo, name, err := repoAndName(c)
	if err != nil {
		return err
	}
	roles, _, err := pipelineRoles(repo, name)
	if err != nil {
		return cli.Exit("prf: "+err.Error(), exitUsage)
	}
	if c.Bool("json") {
		p, _, _ := readPipelines(repo)
		e := pipelineEntry{Name: name, Steps: roles, Default: name == p.Default}
		if err := checkPipeline(repo, roles); err != nil {
			e.Error = err.Error()
		}
		e.Valid = e.Error == ""
		return writeJSON(c.Root().Writer, e)
	}
	_, err = fmt.Fprintln(c.Root().Writer, joinSteps(roles, "\n"))
	return err
}

func pipelineSet(ctx context.Context, c *cli.Command) error {
	repo, name, err := repoAndName(c)
	if err != nil {
		return err
	}
	text := c.String("roles")
	if text == "" {
		var b []byte
		if f := c.String("file"); f != "" {
			b, err = os.ReadFile(f)
		} else {
			b, err = io.ReadAll(c.Root().Reader)
		}
		if err != nil {
			return cli.Exit("prf: reading roles: "+err.Error(), exitUsage)
		}
		text = string(b)
	}
	roles, err := parseRoleList(text)
	if err == nil {
		err = checkPipeline(repo, roles)
	}
	if err != nil {
		return cli.Exit(fmt.Sprintf("prf: not saved: pipeline %s: %v", name, err), exitUsage)
	}

	p, exists, err := readPipelines(repo)
	if err != nil {
		return cli.Exit("prf: "+err.Error(), exitUsage)
	}
	if !exists {
		p = pipelines{Pipelines: map[string][]step{}} // don't persist the built-in default
	}
	p.Pipelines[name] = roles
	if c.Bool("default") || p.Default == "" {
		p.Default = name
	}
	if err := writePipelines(repo, p); err != nil {
		return cli.Exit("prf: "+err.Error(), exitSetup)
	}
	_, _ = fmt.Fprintf(c.Root().Writer, "saved pipeline %s for %s: %s\n", name, repo, joinSteps(roles, " -> "))
	return nil
}

func pipelineRemove(ctx context.Context, c *cli.Command) error {
	repo, name, err := repoAndName(c)
	if err != nil {
		return err
	}
	p, exists, err := readPipelines(repo)
	if err != nil {
		return cli.Exit("prf: "+err.Error(), exitUsage)
	}
	if _, ok := p.Pipelines[name]; !exists || !ok {
		return cli.Exit(fmt.Sprintf("prf: repo %s has no pipeline %q in %s", repo, name, pipelinePath(repo)), exitUsage)
	}
	delete(p.Pipelines, name)
	if p.Default == name {
		p.Default = ""
		logf("warning: %s was the default; run prf pipeline default to pick another", name)
	}
	if err := writePipelines(repo, p); err != nil {
		return cli.Exit("prf: "+err.Error(), exitSetup)
	}
	_, _ = fmt.Fprintf(c.Root().Writer, "removed pipeline %s for %s\n", name, repo)
	return nil
}

func pipelineDefault(ctx context.Context, c *cli.Command) error {
	repo, name, err := repoAndName(c)
	if err != nil {
		return err
	}
	p, exists, err := readPipelines(repo)
	if err != nil {
		return cli.Exit("prf: "+err.Error(), exitUsage)
	}
	if _, ok := p.Pipelines[name]; !exists || !ok {
		return cli.Exit(fmt.Sprintf("prf: repo %s has no pipeline %q in %s", repo, name, pipelinePath(repo)), exitUsage)
	}
	p.Default = name
	if err := writePipelines(repo, p); err != nil {
		return cli.Exit("prf: "+err.Error(), exitSetup)
	}
	_, _ = fmt.Fprintf(c.Root().Writer, "default pipeline for %s is now %s\n", repo, name)
	return nil
}

// repoArg returns a valid --repo.
func repoArg(c *cli.Command) (string, error) {
	repo := c.String("repo")
	if !itemName.MatchString(repo) {
		return "", cli.Exit(fmt.Sprintf("prf: invalid repo name %q: use letters, digits, '-', '_' and '.'", repo), exitUsage)
	}
	return repo, nil
}

// repoAndName returns --repo and the single <name> argument.
func repoAndName(c *cli.Command) (repo, name string, err error) {
	if repo, err = repoArg(c); err != nil {
		return "", "", err
	}
	name, err = nameArg(c, "pipeline")
	return repo, name, err
}

// jobRoles picks the steps to run: --role, else --pipeline from the repo's
// pipeline.json, else (when useDefault) its default pipeline. It also
// returns a name for them: the pipeline's, "roles" for --role, or "" for none.
func jobRoles(c *cli.Command, repo string, useDefault bool) ([]step, string, error) {
	if c.IsSet("role") {
		if c.IsSet("pipeline") {
			return nil, "", errors.New("give --role or --pipeline, not both")
		}
		steps, err := parseRoleList(strings.Join(c.StringSlice("role"), ","))
		if err != nil {
			return nil, "", fmt.Errorf("--role: %w", err)
		}
		return steps, "roles", nil
	}
	name := c.String("pipeline")
	if name == "" && !useDefault {
		return nil, "", nil
	}
	return pipelineRoles(repo, name)
}
