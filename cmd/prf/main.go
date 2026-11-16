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
	"slices"
	"strings"
	"sync"

	"github.com/chrisrob11/prfoundry/internal/config"
	"github.com/chrisrob11/prfoundry/internal/gitx"
	"github.com/chrisrob11/prfoundry/internal/itemname"
	"github.com/chrisrob11/prfoundry/internal/jobenv"
	"github.com/chrisrob11/prfoundry/internal/mise"
	"github.com/chrisrob11/prfoundry/internal/pipeline"
	"github.com/chrisrob11/prfoundry/internal/roles"
	"github.com/chrisrob11/prfoundry/internal/sandbox"
	"github.com/urfave/cli/v3"
)

const (
	exitUsage  = 2
	exitSetup  = 3
	exitShip   = 4  // every step passed, but pushing or opening the PR failed
	exitBudget = 5  // a cost cap stopped the job
	exitReview = 6  // a review gate ran out of rounds with changes still requested
	exitAsk    = 7  // an ask gate ran out of rounds with the step still asking
	exitOwner  = 8  // you stopped the job at an ask gate
	exitCheck  = 9  // a check gate's command still failed after max_rounds fixes
	exitExpect = 10 // a role step didn't leave the checkout as its expect says
)

func main() {
	if err := newApp().Run(context.Background(), os.Args); err != nil {
		code := exitUsage
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

func newApp() *cli.Command {
	a := &app{}
	return &cli.Command{
		Name:  "prf",
		Usage: "run many PRs at once, each through a pipeline of Claude Code roles",
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

// stderr is where progress, status lines and warnings go; tests replace it.
var stderr io.Writer = os.Stderr

func logf(format string, args ...any) {
	_, _ = fmt.Fprintf(stderr, "prf: "+format+"\n", args...)
}

// job is everything one prf invocation needs, after flags, env, and the
// config file have been merged.
type job struct {
	home         string        // the prf home dir
	cfg          config.Config // conf.json
	name         string        // short repo name
	workdir      string
	jobDir       string // <workdir>/<name>/<pr or branch>/: holds status and logs
	dir          string // where Claude runs
	branch       string
	pr           string
	prDir        string
	task         string
	roles        []pipeline.Step
	pipeline     string  // name shown in progress and status lines
	model        string  // --model: overrides every step's model
	defaultModel string  // for steps that don't name a model
	maxCost      float64 // the whole job's budget in USD; 0 means none
	stepCost     float64 // --max-step-cost: overrides every step's budget
	defaultStep  float64 // budget for steps that don't set max_cost
	expect       string  // for role steps that don't set expect
	push         bool    // push the branch once every step passes
	prCreate     string  // then open a PR: "draft", "ready", or ""
	base         string  // the PR's base branch; "" lets gh pick
	prTitleFile  string
	prBodyFile   string
	firstStep    int // steps already done before this run (resume)
	totalSteps   int // steps in the whole pipeline (resume); 0 means len(phases)
	priorCost    float64
	verbose      bool // also print each of Claude's steps to stderr
	out          string
	tee          bool      // also print Claude's summary of each phase to stdout
	stdout       io.Writer // the command's stdout
	status       string
	settings     string   // Claude settings file; empty when the sandbox is off
	env          []string // environment for mise and Claude
	tmp          string   // <job>/tmp: the steps' PRF_TMP and TMPDIR
	writeDirs    []string // folders under the job folder that job_env and job_seed use
	checkCmd     string   // the repo's check command, for @check gates without their own
	contextDir   string   // <job>/context: codebase map and setup output
	contextFiles []string // files in contextDir, listed in every prompt
	stateMu      sync.Mutex
	state        jobState // what's in <job>/state.json
	in           string
	mode         string
	claude       string
	claudeArgs   []string
	reviewOpen   bool // open the review page in your browser when a review gate is reached

	// Set by runPhases.
	log, raw   io.Writer // Claude's step-by-step log, and its raw stream
	feed       io.Writer // the log, plus stderr with --verbose
	summaries  io.Writer // where each phase's final message goes
	sigs       chan os.Signal
	cost       float64     // spent so far, including before a resume
	lastResult phaseResult // the last Claude run's result
}

// newJob fills in the settings shared by run and apply.
func (a *app) newJob(c *cli.Command, repo config.Repo, roles []pipeline.Step) job {
	return job{
		home:       a.home,
		cfg:        a.cfg,
		pr:         c.String("pr"),
		prDir:      firstNonEmpty(repo.PRDir, a.cfg.PRDir),
		task:       c.String("prompt"),
		roles:      roles,
		verbose:    c.Bool("verbose"),
		model:      c.String("model"),
		stepCost:   c.Float64("max-step-cost"),
		stdout:     c.Root().Writer,
		in:         c.String("in"),
		mode:       flagOr(c, "mode", a.cfg.Mode),
		claude:     resolveBinary(flagOr(c, "claude", a.cfg.Claude)),
		claudeArgs: append(append([]string{}, a.cfg.ClaudeArgs...), c.Args().Slice()...),
		reviewOpen: a.reviewOpen(c),
	}
}

// reviewOpen reports whether a review gate opens its page in the browser:
// --review-open if given, else conf.json's review_open, else true.
func (a *app) reviewOpen(c *cli.Command) bool {
	if c.IsSet("review-open") {
		return c.Bool("review-open")
	}
	return a.cfg.ReviewOpen == nil || *a.cfg.ReviewOpen
}

// prepare resolves the PR file and loads every role before any git work
// happens, so a typo fails fast.
func (j *job) prepare() ([]phase, error) {
	pr, err := resolvePR(j.pr, j.prDir)
	if err != nil {
		return nil, err
	}
	j.pr = pr
	phases, err := loadPhases(j.roles, roles.Dirs(j.home, j.name), j.pr != "" || j.task != "", j.task != "")
	if err != nil {
		return nil, err
	}
	return phases, pipeline.CheckCommands(j.roles, j.checkCmd)
}

// setJobDir places the job folder under workdir, named after the PR if there
// is one and the branch otherwise, points --out and --status into it unless
// they were given, and finds the Claude settings file.
func (j *job) setJobDir(c *cli.Command) error {
	workdir, err := filepath.Abs(flagOr(c, "workdir", j.cfg.Workdir))
	if err != nil {
		return err
	}
	leaf := strings.ReplaceAll(j.branch, "/", "-")
	if j.pr != "" {
		leaf = strings.TrimSuffix(filepath.Base(j.pr), ".md")
	}
	j.workdir, j.jobDir = workdir, filepath.Join(workdir, j.name, leaf)

	j.out, j.tee = c.String("out"), false
	if j.out == "" {
		j.out, j.tee = filepath.Join(j.jobDir, "claude.log"), true
	}
	j.status = firstNonEmpty(c.String("status"), filepath.Join(j.jobDir, "status"))
	return j.useSandbox(c)
}

// useSandbox picks the Claude settings file, unless the sandbox is off.
func (j *job) useSandbox(c *cli.Command) error {
	on := j.cfg.Sandbox == nil || *j.cfg.Sandbox
	if c.IsSet("sandbox") {
		on = c.Bool("sandbox")
	}
	j.settings = ""
	if !on {
		return nil
	}
	var err error
	j.settings, err = sandbox.SettingsPath(j.home, j.name, logf)
	return err
}

func (a *app) runCmd(ctx context.Context, c *cli.Command) error {
	repoArg := c.String("repo")
	if repoArg == "" {
		return cli.Exit("prf: --repo is required", exitUsage)
	}
	url, name := repoArg, strings.TrimSuffix(filepath.Base(repoArg), ".git")
	repo, isAlias := a.cfg.Repos[repoArg]
	if isAlias {
		if repo.URL == "" {
			return cli.Exit(fmt.Sprintf("prf: repo alias %q has no url", repoArg), exitUsage)
		}
		url, name = repo.URL, repoArg
	}

	if !itemname.Valid(name) {
		return cli.Exit(fmt.Sprintf("prf: can't use %q as a repo name; add an alias for it in %s", name, config.FileName), exitUsage)
	}
	roles, pipelineName, err := jobRoles(c, a.home, name, true)
	if err != nil {
		return cli.Exit("prf: "+err.Error(), exitUsage)
	}
	j := a.newJob(c, repo, roles)
	j.name, j.pipeline = name, pipelineName
	if err := j.applyRepoDefaults(c); err != nil {
		return cli.Exit("prf: "+err.Error(), exitUsage)
	}
	phases, err := j.prepare()
	if err != nil {
		return cli.Exit("prf: "+err.Error(), exitUsage)
	}

	j.branch = c.String("branch")
	if j.branch == "" {
		if j.pr == "" {
			return cli.Exit("prf: --branch is required without --pr", exitUsage)
		}
		j.branch = a.cfg.BranchPrefix + strings.TrimSuffix(filepath.Base(j.pr), ".md")
	}
	if err := j.setJobDir(c); err != nil {
		return cli.Exit("prf: "+err.Error(), exitSetup)
	}
	j.dir = filepath.Join(j.jobDir, "repo")
	// Clones under workdir are ours, so mise may trust their config files.
	j.env = mise.Env(j.workdir)

	// An earlier job's folder is left alone: its status and state stay as they were.
	if _, err := os.Stat(j.dir); err == nil {
		return cli.Exit(fmt.Sprintf("prf: %s already exists from an earlier job; remove %s to start over, or resume it with prf apply --repo %s --dir %s", j.dir, j.jobDir, j.name, j.dir), exitSetup)
	}
	if err := checkDisk(j.workdir, a.cfg.MaxDisk); err != nil {
		return cli.Exit("prf: "+err.Error(), exitSetup)
	}
	j.setupStage("cloning")
	if err := cloneRepo(url, flagOr(c, "base", repo.Base), j.branch, j.dir, repo.Path); err != nil {
		logf("%v", err)
		return j.finish("setup", exitSetup)
	}
	if err := jobenv.Write(j.jobDir, j.name, repo.JobEnv); err != nil {
		logf("writing the job's mise.toml: %v", err)
		return j.finish("setup", exitSetup)
	}
	if len(repo.JobSeed) > 0 {
		j.setupStage("seeding the job folder")
		seeds := slices.Clone(repo.JobSeed)
		for i := range seeds {
			seeds[i].From = config.ExpandHome(seeds[i].From)
		}
		jobenv.Copy(j.jobDir, seeds, logf)
	}
	j.setupStage("installing tools")
	mise.Install(j.dir, j.env, logf)
	if err := j.prepareContext(); err != nil {
		logf("%v", err)
		return j.finish("setup", exitSetup)
	}
	return j.runPhases(phases)
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
			return cli.Exit(fmt.Sprintf("prf: repo %q has no path in %s; give --dir", repoName, config.FileName), exitUsage)
		}
		dir = repo.Path
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return cli.Exit("prf: "+err.Error(), exitUsage)
	}
	name := firstNonEmpty(repoName, filepath.Base(dir))
	if !itemname.Valid(name) {
		return cli.Exit(fmt.Sprintf("prf: can't use %q as a repo name; add an alias for it in %s", name, config.FileName), exitUsage)
	}

	roles, pipelineName, err := jobRoles(c, a.home, name, false)
	if err != nil {
		return cli.Exit("prf: "+err.Error(), exitUsage)
	}
	j := a.newJob(c, repo, roles)
	j.name, j.dir, j.pipeline = name, dir, pipelineName
	if err := j.applyRepoDefaults(c); err != nil {
		return cli.Exit("prf: "+err.Error(), exitUsage)
	}
	phases, err := j.prepare()
	if err != nil {
		return cli.Exit("prf: "+err.Error(), exitUsage)
	}
	if !gitx.OK(j.dir, "rev-parse", "--is-inside-work-tree") {
		return cli.Exit("prf: not a git repository: "+j.dir, exitUsage)
	}

	if b := c.String("branch"); b != "" {
		if err := switchBranch(j.dir, b); err != nil {
			logf("%v", err)
			j.branch = b
			if err := j.setJobDir(c); err != nil {
				return cli.Exit("prf: "+err.Error(), exitSetup)
			}
			return j.finish("setup", exitSetup)
		}
	}
	j.branch = firstNonEmpty(gitx.Output(j.dir, "branch", "--show-current"), "HEAD")
	if err := j.setJobDir(c); err != nil {
		return cli.Exit("prf: "+err.Error(), exitSetup)
	}
	if gitx.Output(j.dir, "status", "--porcelain") != "" {
		logf("warning: %s has uncommitted changes; Claude's commits may include them", j.dir)
	}
	j.env = mise.Env(mise.TrustedDir(j.dir, j.workdir)) // your own checkout keeps mise's trust as you set it
	j.setupStage("installing tools")
	mise.Install(j.dir, j.env, logf)
	if err := j.prepareContext(); err != nil {
		logf("%v", err)
		return j.finish("setup", exitSetup)
	}
	return j.runPhases(phases)
}

// applyRepoDefaults fills in the model, cost caps, expectation, and shipping
// settings that come from the repo's pipeline.json and conf.json, under the
// flags.
func (j *job) applyRepoDefaults(c *cli.Command) error {
	p, _, _ := pipeline.Read(j.home, j.name)
	j.defaultModel = firstNonEmpty(p.Model, j.cfg.Model)
	j.maxCost = firstPositive(c.Float64("max-cost"), p.MaxCost, j.cfg.MaxCost)
	j.defaultStep = firstPositive(p.MaxStepCost, j.cfg.MaxStepCost)
	j.checkCmd, j.expect = p.Check, p.Expect

	repo := j.cfg.Repos[j.name]
	j.push, j.prCreate = repo.Push, repo.PRCreate
	j.base, j.prTitleFile, j.prBodyFile = flagOr(c, "base", repo.Base), repo.PRTitleFile, repo.PRBodyFile
	if c.IsSet("pr-create") {
		switch v := c.String("pr-create"); v {
		case config.PRDraft, config.PRReady:
			j.prCreate = v
		case prNone:
			j.prCreate = ""
		default:
			return fmt.Errorf("--pr-create must be %s, %s or %s", config.PRDraft, config.PRReady, prNone)
		}
	}
	if j.prCreate != "" {
		j.push = true // a PR needs the branch pushed
	}
	if c.IsSet("push") && !c.Bool("push") {
		j.push, j.prCreate = false, ""
	}
	return nil
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
