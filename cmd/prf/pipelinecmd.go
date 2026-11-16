package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/chrisrob11/prfoundry/internal/itemname"
	"github.com/chrisrob11/prfoundry/internal/jsonio"
	"github.com/chrisrob11/prfoundry/internal/pipeline"
	"github.com/chrisrob11/prfoundry/internal/roles"
	"github.com/urfave/cli/v3"
)

// checkPipeline reports the first step that names a missing or invalid role,
// or an invalid model, for repo, or a @check with no command to run.
func checkPipeline(home, repo string, steps []pipeline.Step) error {
	if len(steps) == 0 {
		return errors.New("lists no roles")
	}
	if err := pipeline.CheckAskers(steps); err != nil {
		return err
	}
	p, _, _ := pipeline.Read(home, repo) // a broken file is reported by whoever read the steps from it
	if err := pipeline.CheckCommands(steps, p.Check); err != nil {
		return err
	}
	for _, s := range steps {
		if s.Model != "" && !pipeline.ValidModel(s.Model) {
			return fmt.Errorf("step %s: invalid model %q", s.Name(), s.Model)
		}
		if s.MaxCost < 0 {
			return fmt.Errorf("step %s: max_cost can't be negative", s.Name())
		}
		if err := pipeline.CheckGate(s); err != nil {
			return err
		}
		role := s.Role
		if s.Gate != "" {
			role = s.Fixer() // the gate needs its fix role
		}
		text, err := roles.Find(roles.Dirs(home, repo), role)
		if err == nil {
			err = roles.Validate(role, text)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// pipelineEntry describes one pipeline as list and show report it.
type pipelineEntry struct {
	Name    string          `json:"name"`
	Steps   []pipeline.Step `json:"steps"`
	Default bool            `json:"default"`
	Valid   bool            `json:"valid"`
	Error   string          `json:"error,omitempty"`
}

func (a *app) pipelineCmd() *cli.Command {
	repoFlag := &cli.StringFlag{Name: "repo", Required: true, Usage: "repo whose " + pipeline.FileName + " to use (an alias or repo name)"}
	jsonFlag := &cli.BoolFlag{Name: "json", Usage: "print JSON"}
	return &cli.Command{
		Name:  "pipeline",
		Usage: "list, show, and change a repo's pipelines; built for LLMs and scripts as well as people",
		Description: "A repo's pipelines live in ~/.prf/<repo>/" + pipeline.FileName + ": named lists of steps,\n" +
			"run in order, plus the default one. A step is a role, optionally with the model it\n" +
			"runs on: role:model here (e.g. implement:opus), {\"role\": ..., \"model\": ...} in the file.\n" +
			"@review is a human review gate: the job waits for your review in the browser, and its fix role\n" +
			"addresses your comments until you approve. @ask, after a role, shows you the question that role\n" +
			"left ($PRF_TMP/stop.md) beside the diff, records your answers, and re-runs the role.\n" +
			"@check runs a command (its \"run\", or the file's \"check\") and has its fix role fix what fails;\n" +
			"A repo without the file has a built-in \"default\" pipeline: " + pipeline.Join(pipeline.Builtin(), ", ") + ".\n" +
			"set refuses a pipeline that names a missing or invalid role, and leaves the file unchanged.",
		Commands: []*cli.Command{
			{Name: "list", Usage: "list the repo's pipelines, which is the default, and whether each is valid", Flags: []cli.Flag{repoFlag, jsonFlag}, Action: a.pipelineList},
			{Name: "show", Usage: "print a pipeline's steps (role or role:model), one per line", ArgsUsage: "<name>", Flags: []cli.Flag{repoFlag, jsonFlag}, Action: a.pipelineShow},
			{Name: "set", Usage: "create or replace a pipeline from --roles, --file, or stdin (steps one per line or comma-separated)", ArgsUsage: "<name>",
				Flags: []cli.Flag{repoFlag,
					&cli.StringFlag{Name: "roles", Usage: "comma-separated steps (role or role:model), instead of stdin"},
					&cli.StringFlag{Name: "file", Usage: "read roles from this file instead of stdin"},
					&cli.BoolFlag{Name: "default", Usage: "also make it the default pipeline"},
				}, Action: a.pipelineSet},
			{Name: "remove", Usage: "delete a pipeline", ArgsUsage: "<name>", Flags: []cli.Flag{repoFlag}, Action: a.pipelineRemove},
			{Name: "default", Usage: "make a pipeline the one that runs when --pipeline isn't given", ArgsUsage: "<name>", Flags: []cli.Flag{repoFlag}, Action: a.pipelineDefault},
		},
	}
}

func (a *app) pipelineList(ctx context.Context, c *cli.Command) error {
	repo, err := repoArg(c)
	if err != nil {
		return err
	}
	p, exists, err := pipeline.Read(a.home, repo)
	if err != nil {
		return cli.Exit("prf: "+err.Error(), exitUsage)
	}
	var entries []pipelineEntry
	for _, name := range slices.Sorted(maps.Keys(p.Pipelines)) {
		e := pipelineEntry{Name: name, Steps: p.Pipelines[name], Default: name == p.Default}
		if err := checkPipeline(a.home, repo, e.Steps); err != nil {
			e.Error = err.Error()
		}
		e.Valid = e.Error == ""
		entries = append(entries, e)
	}

	w := c.Root().Writer
	if c.Bool("json") {
		return jsonio.Write(w, entries)
	}
	if !exists {
		_, _ = fmt.Fprintf(w, "# %s doesn't exist; using the built-in default\n", pipeline.Path(a.home, repo))
	}
	if m := firstNonEmpty(p.Model, a.cfg.Model); m != "" {
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
		_, _ = fmt.Fprintf(w, "%s %-12s %s%s\n", mark, e.Name, pipeline.Join(e.Steps, " -> "), status)
	}
	return nil
}

func (a *app) pipelineShow(ctx context.Context, c *cli.Command) error {
	repo, name, err := repoAndName(c)
	if err != nil {
		return err
	}
	roles, _, err := pipeline.Lookup(a.home, repo, name)
	if err != nil {
		return cli.Exit("prf: "+err.Error(), exitUsage)
	}
	if c.Bool("json") {
		p, _, _ := pipeline.Read(a.home, repo)
		e := pipelineEntry{Name: name, Steps: roles, Default: name == p.Default}
		if err := checkPipeline(a.home, repo, roles); err != nil {
			e.Error = err.Error()
		}
		e.Valid = e.Error == ""
		return jsonio.Write(c.Root().Writer, e)
	}
	_, err = fmt.Fprintln(c.Root().Writer, pipeline.Join(roles, "\n"))
	return err
}

func (a *app) pipelineSet(ctx context.Context, c *cli.Command) error {
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
	roles, err := pipeline.ParseSteps(text)
	if err == nil {
		err = checkPipeline(a.home, repo, roles)
	}
	if err != nil {
		return cli.Exit(fmt.Sprintf("prf: not saved: pipeline %s: %v", name, err), exitUsage)
	}

	p, exists, err := pipeline.Read(a.home, repo)
	if err != nil {
		return cli.Exit("prf: "+err.Error(), exitUsage)
	}
	if !exists {
		p = pipeline.File{Pipelines: map[string][]pipeline.Step{}} // don't persist the built-in default
	}
	p.Pipelines[name] = roles
	if c.Bool("default") || p.Default == "" {
		p.Default = name
	}
	if err := pipeline.Write(a.home, repo, p); err != nil {
		return cli.Exit("prf: "+err.Error(), exitSetup)
	}
	_, _ = fmt.Fprintf(c.Root().Writer, "saved pipeline %s for %s: %s\n", name, repo, pipeline.Join(roles, " -> "))
	return nil
}

func (a *app) pipelineRemove(ctx context.Context, c *cli.Command) error {
	repo, name, err := repoAndName(c)
	if err != nil {
		return err
	}
	p, exists, err := pipeline.Read(a.home, repo)
	if err != nil {
		return cli.Exit("prf: "+err.Error(), exitUsage)
	}
	if _, ok := p.Pipelines[name]; !exists || !ok {
		return cli.Exit(fmt.Sprintf("prf: repo %s has no pipeline %q in %s", repo, name, pipeline.Path(a.home, repo)), exitUsage)
	}
	delete(p.Pipelines, name)
	if p.Default == name {
		p.Default = ""
		logf("warning: %s was the default; run prf pipeline default to pick another", name)
	}
	if err := pipeline.Write(a.home, repo, p); err != nil {
		return cli.Exit("prf: "+err.Error(), exitSetup)
	}
	_, _ = fmt.Fprintf(c.Root().Writer, "removed pipeline %s for %s\n", name, repo)
	return nil
}

func (a *app) pipelineDefault(ctx context.Context, c *cli.Command) error {
	repo, name, err := repoAndName(c)
	if err != nil {
		return err
	}
	p, exists, err := pipeline.Read(a.home, repo)
	if err != nil {
		return cli.Exit("prf: "+err.Error(), exitUsage)
	}
	if _, ok := p.Pipelines[name]; !exists || !ok {
		return cli.Exit(fmt.Sprintf("prf: repo %s has no pipeline %q in %s", repo, name, pipeline.Path(a.home, repo)), exitUsage)
	}
	p.Default = name
	if err := pipeline.Write(a.home, repo, p); err != nil {
		return cli.Exit("prf: "+err.Error(), exitSetup)
	}
	_, _ = fmt.Fprintf(c.Root().Writer, "default pipeline for %s is now %s\n", repo, name)
	return nil
}

// repoArg returns a valid --repo.
func repoArg(c *cli.Command) (string, error) {
	repo := c.String("repo")
	if !itemname.Valid(repo) {
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
func jobRoles(c *cli.Command, home, repo string, useDefault bool) ([]pipeline.Step, string, error) {
	if c.IsSet("role") {
		if c.IsSet("pipeline") {
			return nil, "", errors.New("give --role or --pipeline, not both")
		}
		steps, err := pipeline.ParseSteps(strings.Join(c.StringSlice("role"), ","))
		if err != nil {
			return nil, "", fmt.Errorf("--role: %w", err)
		}
		return steps, "roles", nil
	}
	name := c.String("pipeline")
	if name == "" && !useDefault {
		return nil, "", nil
	}
	return pipeline.Lookup(home, repo, name)
}
