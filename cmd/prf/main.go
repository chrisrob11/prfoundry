// prf runs Claude headless through a sequence of role-based phases, either
// in a fresh clone of a repo (run) or in a repo that already exists (apply).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/chrisrob11/prfoundry/internal/config"
	"github.com/chrisrob11/prfoundry/internal/itemname"
	"github.com/chrisrob11/prfoundry/internal/job"
	"github.com/chrisrob11/prfoundry/internal/pipeline"
	"github.com/chrisrob11/prfoundry/internal/sandbox"
	"github.com/urfave/cli/v3"
)

// prNone is --pr-create's third value, beside config.PRDraft and config.PRReady: open no PR.
const prNone = "none"

func main() {
	if err := newApp(os.Stderr, job.Runtime{}).Run(context.Background(), os.Args); err != nil {
		code := job.ExitUsage
		var ec cli.ExitCoder
		if errors.As(err, &ec) {
			code = ec.ExitCode()
		} else {
			err = fmt.Errorf("prf: %w", err)
		}
		if msg := err.Error(); msg != "" {
			fmt.Fprintln(os.Stderr, msg)
		}
		os.Exit(code)
	}
}

// newApp is prf's command line. Progress, status lines and warnings go to
// stderr, and jobs reach your browser, desktop and disk through rt.
func newApp(stderr io.Writer, rt job.Runtime) *cli.Command {
	a := &app{stderr: stderr, rt: rt}
	return &cli.Command{
		Name:    "prf",
		Usage:   "run many PRs at once, each through a pipeline of Claude Code roles",
		Version: buildVersion().String(),
		// main prints errors and exits; the default handler would call os.Exit itself.
		ExitErrHandler: func(context.Context, *cli.Command, error) {},
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "home", Value: config.DefaultHome(), Sources: cli.EnvVars("PRF_HOME"), Usage: "prf home: conf.json, shared roles/, and a folder per repo with its pipeline.json and roles/"},
		},
		Before: a.loadConfig,
		Commands: []*cli.Command{
			{
				Name:      "run",
				Usage:     "clone a repo into <workdir>/<repo>/<job>/repo, switch to a branch, and run Claude phases on it",
				ArgsUsage: "[-- extra claude args]",
				Flags: append([]cli.Flag{
					&cli.StringFlag{Name: "repo", Usage: "repo URL, path, or alias from conf.json (required)"},
					&cli.StringFlag{Name: "branch", Usage: "branch to create or switch to (default: <branch_prefix><pr name>)"},
					&cli.StringFlag{Name: "base", Usage: "branch to clone from (default: remote default)"},
				}, phaseFlags("the repo's default pipeline")...),
				Action: a.runCmd,
			},
			{
				Name:      "apply",
				Usage:     "run Claude phases in an existing local repo",
				ArgsUsage: "[-- extra claude args]",
				Flags: append([]cli.Flag{
					&cli.StringFlag{Name: "repo", Usage: "repo name (an alias from conf.json works in its path); with --dir, says which repo's pipelines and roles to use (default: the folder name)"},
					&cli.StringFlag{Name: "dir", Value: ".", Usage: "existing repo to work in"},
					&cli.StringFlag{Name: "branch", Usage: "switch to this branch first, creating it if needed (default: stay on the current branch)"},
				}, phaseFlags("none; just --prompt")...),
				Action: a.applyCmd,
			},
			a.statusCmd(),
			a.costsCmd(),
			a.resumeCmd(),
			a.cleanCmd(),
			a.roleCmd(),
			a.pipelineCmd(),
			{
				Name:  "index",
				Usage: "build the codebase map for a checkout, without running Claude (to preview what roles are given)",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "dir", Value: ".", Usage: "checkout to map"},
					&cli.StringFlag{Name: "out", Value: "codebase-map", Usage: "folder to write index.md and packages/ into"},
				},
				Action: indexCmd,
			},
			a.configCmd(),
			versionCmd(),
		},
	}
}

// phaseFlags are the flags shared by run and apply.
func phaseFlags(defaultPipeline string) []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "pr", Usage: "PR spec file; a bare name is also looked up in pr_dir"},
		&cli.StringFlag{Name: "prompt", Usage: "task for Claude; with no --role it is the whole prompt, otherwise it is given to each role"},
		&cli.StringFlag{Name: "pipeline", Usage: "pipeline from ~/.prf/<repo>/" + pipeline.FileName + " to run (default: " + defaultPipeline + ")"},
		&cli.StringSliceFlag{Name: "role", Usage: "roles to run in order instead of a pipeline, one phase each; role:model picks a step's model (e.g. implement:opus)"},
		&cli.StringFlag{Name: "model", Usage: "model for every step, overriding the pipeline's (default: each step's model, else pipeline.json's model, else conf.json's, else Claude's own)"},
		&cli.BoolFlag{Name: "push", Usage: "once every step passes, push the branch (never forced); --push=false skips it, and the PR, this once (default: the repo alias's push)"},
		&cli.StringFlag{Name: "pr-create", Usage: "after pushing, open a PR: draft, ready, or none (default: the repo alias's pr_create); implies --push"},
		&cli.Float64Flag{Name: "max-cost", Usage: "stop the job once it has spent this many USD (default: pipeline.json's max_cost, else conf.json's, else no cap)"},
		&cli.Float64Flag{Name: "max-step-cost", Usage: "budget in USD for every step, overriding each step's max_cost (default: the step's, else pipeline.json's max_step_cost, else conf.json's, else no cap)"},
		&cli.StringFlag{Name: "workdir", Value: config.ExpandHome("~/prf-work"), Sources: cli.EnvVars("PRF_WORKDIR"), Usage: "base directory for job folders: <workdir>/<repo>/<job>/ (default: conf.json's workdir, else this)"},
		&cli.BoolFlag{Name: "verbose", Aliases: []string{"v"}, Usage: "also print each step Claude takes (files read, commands run, edits, messages) as it happens"},
		&cli.StringFlag{Name: "out", Usage: "write Claude's step-by-step log to this file instead of <job>/claude.log, and don't print its summaries to stdout"},
		&cli.StringFlag{Name: "in", Usage: "extra context on Claude's stdin: a file (sent to every phase), or a fifo or '-' (first phase only)"},
		&cli.StringFlag{Name: "status", Usage: "append status lines to this file (default: <job>/status)"},
		&cli.StringFlag{Name: "mode", Value: "acceptEdits", Usage: "Claude permission mode"},
		&cli.BoolFlag{Name: "review-open", Value: true, Usage: "at a @review or @ask gate, open its page in your browser (--review-open=false only notifies; default: conf.json's review_open, else true)"},
		&cli.BoolFlag{Name: "sandbox", Value: true, Usage: "run Claude in Claude Code's sandbox, with ~/.prf/[<repo>/]" + sandbox.SettingsFile + " (--sandbox=false turns it off)"},
		&cli.StringFlag{Name: "claude", Value: "claude", Sources: cli.EnvVars("CLAUDE_BIN"), Usage: "claude binary"},
	}
}

func (a *app) logf(format string, args ...any) {
	_, _ = fmt.Fprintf(a.stderr, "prf: "+format+"\n", args...)
}

// reviewOpen reports whether a review gate opens its page in the browser:
// --review-open if given, else conf.json's review_open, else true.
func (a *app) reviewOpen(c *cli.Command) bool {
	if c.IsSet("review-open") {
		return c.Bool("review-open")
	}
	return a.cfg.ReviewOpen == nil || *a.cfg.ReviewOpen
}

// sandboxOn reports whether Claude runs in its sandbox: --sandbox if given,
// else conf.json's sandbox, else true.
func (a *app) sandboxOn(c *cli.Command) bool {
	if c.IsSet("sandbox") {
		return c.Bool("sandbox")
	}
	return a.cfg.Sandbox == nil || *a.cfg.Sandbox
}

// options are the settings run, apply and resume share for a job in repo
// name: the flags, over the repo's pipeline.json and conf.json.
func (a *app) options(c *cli.Command, name string) (job.Options, error) {
	p, _, _ := pipeline.Read(a.home, name)
	repo := a.cfg.Repos[name]
	o := job.Options{
		Home: a.home, Repo: name,
		Model: c.String("model"), DefaultModel: firstNonEmpty(p.Model, a.cfg.Model),
		MaxCost: firstPositive(c.Float64("max-cost"), p.MaxCost, a.cfg.MaxCost), StepCost: c.Float64("max-step-cost"),
		DefaultStepCost: firstPositive(p.MaxStepCost, a.cfg.MaxStepCost),
		Expect:          p.Expect, Check: p.Check,
		Push: repo.Push, PRCreate: repo.PRCreate, Base: flagOr(c, "base", repo.Base),
		PRTitleFile: repo.PRTitleFile, PRBodyFile: repo.PRBodyFile,
		JobEnv: repo.JobEnv, JobSeed: repo.JobSeed,
		Verbose: c.Bool("verbose"), Mode: flagOr(c, "mode", a.cfg.Mode), Claude: resolveBinary(flagOr(c, "claude", a.cfg.Claude)),
		Sandbox: a.sandboxOn(c), ReviewOpen: a.reviewOpen(c),
		Stdout: c.Root().Writer, Stderr: a.stderr, Runtime: a.rt,
	}
	if c.IsSet("pr-create") {
		switch v := c.String("pr-create"); v {
		case config.PRDraft, config.PRReady:
			o.PRCreate = v
		case prNone:
			o.PRCreate = ""
		default:
			return o, fmt.Errorf("--pr-create must be %s, %s or %s", config.PRDraft, config.PRReady, prNone)
		}
	}
	if o.PRCreate != "" {
		o.Push = true // a PR needs the branch pushed
	}
	if c.IsSet("push") && !c.Bool("push") {
		o.Push, o.PRCreate = false, ""
	}
	return o, nil
}

// newJob makes run's or apply's job, in repo name, from its flags: the
// steps to run, and the PR spec, looked up in prDir too.
func (a *app) newJob(c *cli.Command, name, prDir string, steps []pipeline.Step, pipelineName string) (*job.Job, job.Options, error) {
	o, err := a.options(c, name)
	if err != nil {
		return nil, o, err
	}
	o.Steps, o.Pipeline, o.Task = steps, pipelineName, c.String("prompt")
	if o.PR, err = resolvePR(c.String("pr"), prDir); err != nil {
		return nil, o, err
	}
	o.Workdir, o.Out, o.Status, o.In = flagOr(c, "workdir", a.cfg.Workdir), c.String("out"), c.String("status"), c.String("in")
	o.ClaudeArgs = append(append([]string{}, a.cfg.ClaudeArgs...), c.Args().Slice()...)
	j, err := job.New(o)
	return j, o, err
}

func (a *app) runCmd(ctx context.Context, c *cli.Command) error {
	repoArg := c.String("repo")
	if repoArg == "" {
		return cli.Exit("prf: --repo is required", job.ExitUsage)
	}
	url, name := repoArg, strings.TrimSuffix(filepath.Base(repoArg), ".git")
	repo, isAlias := a.cfg.Repos[repoArg]
	if isAlias {
		if repo.URL == "" {
			return cli.Exit(fmt.Sprintf("prf: repo alias %q has no url", repoArg), job.ExitUsage)
		}
		url, name = repo.URL, repoArg
	}

	if !itemname.Valid(name) {
		return cli.Exit(fmt.Sprintf("prf: can't use %q as a repo name; add an alias for it in %s", name, config.FileName), job.ExitUsage)
	}
	steps, pipelineName, err := jobRoles(c, a.home, name, true)
	if err != nil {
		return cli.Exit("prf: "+err.Error(), job.ExitUsage)
	}
	j, o, err := a.newJob(c, name, firstNonEmpty(repo.PRDir, a.cfg.PRDir), steps, pipelineName)
	if err != nil {
		return cli.Exit("prf: "+err.Error(), job.ExitUsage)
	}

	branch := c.String("branch")
	if branch == "" {
		if o.PR == "" {
			return cli.Exit("prf: --branch is required without --pr", job.ExitUsage)
		}
		branch = a.cfg.BranchPrefix + strings.TrimSuffix(filepath.Base(o.PR), ".md")
	}
	return j.Run(job.Clone{
		URL: url, Base: flagOr(c, "base", repo.Base), Branch: branch, Reference: repo.Path,
		JobEnv: repo.JobEnv, JobSeed: repo.JobSeed, MaxDisk: a.cfg.MaxDisk,
	})
}

func (a *app) applyCmd(ctx context.Context, c *cli.Command) error {
	// --repo names the repo (for its pipelines, roles and job folder); its
	// alias's path is the checkout unless --dir points somewhere else, such as
	// the clone of an earlier run.
	repoName := c.String("repo")
	repo := a.cfg.Repos[repoName]
	dir := c.String("dir")
	if repoName != "" && !c.IsSet("dir") {
		if repo.Path == "" {
			return cli.Exit(fmt.Sprintf("prf: repo %q has no path in %s; give --dir", repoName, config.FileName), job.ExitUsage)
		}
		dir = repo.Path
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return cli.Exit("prf: "+err.Error(), job.ExitUsage)
	}
	name := firstNonEmpty(repoName, filepath.Base(dir))
	if !itemname.Valid(name) {
		return cli.Exit(fmt.Sprintf("prf: can't use %q as a repo name; add an alias for it in %s", name, config.FileName), job.ExitUsage)
	}

	steps, pipelineName, err := jobRoles(c, a.home, name, false)
	if err != nil {
		return cli.Exit("prf: "+err.Error(), job.ExitUsage)
	}
	j, _, err := a.newJob(c, name, firstNonEmpty(repo.PRDir, a.cfg.PRDir), steps, pipelineName)
	if err != nil {
		return cli.Exit("prf: "+err.Error(), job.ExitUsage)
	}
	return j.Apply(dir, c.String("branch"))
}

func firstPositive(vals ...float64) float64 {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}
	return 0
}

// resolveBinary makes a binary given as a path (./claude, bin/claude)
// absolute, so it means the same thing once Claude runs in the checkout. A
// bare name is left for PATH lookup.
func resolveBinary(name string) string {
	if !strings.ContainsRune(name, filepath.Separator) {
		return name
	}
	if abs, err := filepath.Abs(name); err == nil {
		return abs
	}
	return name
}

// resolvePR returns the absolute path of the PR spec p, trying it as given and
// then as <prDir>/<p> and <prDir>/<p>.md.
func resolvePR(p, prDir string) (string, error) {
	if p == "" {
		return "", nil
	}
	candidates := []string{p}
	if prDir != "" && !filepath.IsAbs(p) {
		candidates = append(candidates, filepath.Join(prDir, p), filepath.Join(prDir, p+".md"))
	}
	for _, cand := range candidates {
		if fi, err := os.Stat(cand); err == nil && fi.Mode().IsRegular() {
			return filepath.Abs(cand)
		}
	}
	return "", fmt.Errorf("PR file not found: %s (also looked in pr_dir %q)", p, prDir)
}
