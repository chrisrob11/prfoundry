package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/chrisrob11/prfoundry/internal/config"
	"github.com/chrisrob11/prfoundry/internal/gitx"
	"github.com/chrisrob11/prfoundry/internal/mise"
	"github.com/chrisrob11/prfoundry/internal/pipeline"
	"github.com/urfave/cli/v3"
)

// findJob returns the state of the job named by target: <repo>/<job> under
// workdir, or a job folder's path.
func findJob(workdir, target string) (jobState, error) {
	jobDir := target
	if _, err := os.Stat(filepath.Join(jobDir, stateFile)); err != nil {
		jobDir = filepath.Join(workdir, target)
	}
	var s jobState
	b, err := os.ReadFile(filepath.Join(jobDir, stateFile))
	if err != nil {
		return s, fmt.Errorf("no job %q: give <repo>/<job> as prf status shows it, or a job folder", target)
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("%s: %w", filepath.Join(jobDir, stateFile), err)
	}
	// A job folder found from the working directory is made absolute: steps
	// run in its repo folder, where a relative path names something else.
	if s.JobDir, err = filepath.Abs(jobDir); err != nil {
		return s, err
	}
	return s, nil
}

func resumeCmd() *cli.Command {
	return &cli.Command{
		Name:      "resume",
		Usage:     "re-run a failed or died job from the step it stopped at, in its existing checkout",
		ArgsUsage: "<repo>/<job> [-- extra claude args]",
		Description: "Uses the job's state.json: same checkout, branch, PR spec, prompt, pipeline and models,\n" +
			"and the current role files. The setup (clone, tools, codebase map, setup commands) isn't\n" +
			"repeated. Status lines are added to the job's status file, and steps keep their numbers.",
		Flags: []cli.Flag{
			&cli.IntFlag{Name: "from", Usage: "step to start from, counting from 1 (default: the step it stopped at)"},
			&cli.StringFlag{Name: "workdir", Value: config.ExpandHome("~/prf-work"), Sources: cli.EnvVars("PRF_WORKDIR"), Usage: "where the job folders are (default: conf.json's workdir, else this)"},
			&cli.BoolFlag{Name: "verbose", Aliases: []string{"v"}, Usage: "also print each step Claude takes"},
			&cli.StringFlag{Name: "model", Usage: "model for every remaining step (default: the models the job used)"},
			&cli.Float64Flag{Name: "max-cost", Usage: "cap for the whole job, counting what it already spent"},
			&cli.Float64Flag{Name: "max-step-cost", Usage: "budget for every remaining step"},
			&cli.BoolFlag{Name: "push", Usage: "push once the remaining steps pass (default: what the job was started with)"},
			&cli.StringFlag{Name: "pr-create", Usage: "after pushing, open a PR: draft, ready, or none (default: what the job was started with)"},
			&cli.BoolFlag{Name: "review-open", Value: true, Usage: "at a @review or @ask gate, open its page in your browser (default: conf.json's review_open, else true)"},
			&cli.StringFlag{Name: "mode", Value: "acceptEdits", Usage: "Claude permission mode"},
			&cli.BoolFlag{Name: "sandbox", Value: true, Usage: "run Claude in Claude Code's sandbox (--sandbox=false turns it off)"},
			&cli.StringFlag{Name: "claude", Value: "claude", Sources: cli.EnvVars("CLAUDE_BIN"), Usage: "claude binary"},
		},
		Action: resume,
	}
}

func resume(ctx context.Context, c *cli.Command) error {
	if c.Args().Len() < 1 {
		return cli.Exit("prf: give the job to resume, as <repo>/<job>", exitUsage)
	}
	workdir, err := filepath.Abs(flagOr(c, "workdir", cfg.Workdir))
	if err != nil {
		return cli.Exit("prf: "+err.Error(), exitUsage)
	}
	st, err := findJob(workdir, c.Args().First())
	if err != nil {
		return cli.Exit("prf: "+err.Error(), exitUsage)
	}
	if st.live() && processAlive(st.PID) {
		return cli.Exit(fmt.Sprintf("prf: %s/%s is still running (process %d)", st.Repo, st.Job, st.PID), exitUsage)
	}

	from := c.Int("from")
	if from == 0 {
		switch {
		case st.State == stateDone:
			return cli.Exit("prf: that job finished; use --from <step> to run steps again", exitUsage)
		case st.Step == 0:
			return cli.Exit("prf: that job stopped during setup, before any step; start a new run", exitUsage)
		}
		from = st.Step
	}
	if from < 1 || from > len(st.Roles) {
		return cli.Exit(fmt.Sprintf("prf: --from must be between 1 and %d", len(st.Roles)), exitUsage)
	}
	if _, err := os.Stat(st.Dir); err != nil {
		return cli.Exit("prf: the job's checkout is gone: "+st.Dir, exitUsage)
	}

	// An ask gate re-runs the step before it, so a resume there loads that
	// step too, then drops it.
	first := from - 1
	if len(st.Steps) == len(st.Roles) && st.Steps[first].Gate == pipeline.GateAsk && first > 0 {
		first--
	}
	steps := make([]pipeline.Step, 0, len(st.Roles)-first)
	for i := first; i < len(st.Roles); i++ {
		s, _ := pipeline.ParseStep(st.Roles[i])
		if len(st.Steps) == len(st.Roles) {
			s = st.Steps[i] // keeps a gate's settings
		}
		if i < len(st.Models) {
			s.Model = st.Models[i]
		}
		steps = append(steps, s)
	}
	j := job{
		name: st.Repo, workdir: workdir, jobDir: st.JobDir, dir: st.Dir, branch: st.Branch,
		pr: st.PR, task: st.Task, roles: steps, pipeline: st.Pipeline,
		verbose: c.Bool("verbose"), stdout: c.Root().Writer, tee: true,
		model: c.String("model"), stepCost: c.Float64("max-step-cost"),
		mode:       flagOr(c, "mode", cfg.Mode),
		claude:     resolveBinary(flagOr(c, "claude", cfg.Claude)),
		claudeArgs: append(append([]string{}, cfg.ClaudeArgs...), c.Args().Tail()...),
		out:        filepath.Join(st.JobDir, "claude.log"),
		status:     filepath.Join(st.JobDir, "status"),
		contextDir: filepath.Join(st.JobDir, "context"),
		firstStep:  from - 1, totalSteps: len(st.Roles), priorCost: st.Cost,
		reviewOpen: reviewOpen(c),
	}
	if err := j.applyRepoDefaults(c); err != nil {
		return cli.Exit("prf: "+err.Error(), exitUsage)
	}
	if !c.IsSet("push") && !c.IsSet("pr-create") {
		j.push, j.prCreate = st.Push, st.PRCreate // ship the way the job was started
	}
	if err := j.useSandbox(c); err != nil {
		return cli.Exit("prf: "+err.Error(), exitSetup)
	}
	j.env = mise.Env(mise.TrustedDir(j.dir, workdir))
	if err := j.listContext(); err != nil && !errors.Is(err, fs.ErrNotExist) { // a missing one just means no context
		logf("warning: listing the context folder: %v", err)
	}
	phases, err := loadPhases(steps, roleDirs(j.name), j.pr != "" || j.task != "", j.task != "")
	if err != nil {
		return cli.Exit("prf: "+err.Error(), exitUsage)
	}
	phases, j.roles = phases[from-1-first:], steps[from-1-first:]
	if err := pipeline.CheckCommands(j.roles, j.checkCmd); err != nil {
		return cli.Exit("prf: "+err.Error(), exitUsage)
	}

	st.PID, st.Exit, st.Activity = os.Getpid(), nil, ""
	j.state = st
	logf("resuming %s/%s from step %d/%d (%s)", st.Repo, st.Job, from, len(st.Roles), st.Roles[from-1])
	return j.runPhases(phases)
}

func cleanCmd() *cli.Command {
	return &cli.Command{
		Name:  "clean",
		Usage: "remove finished job folders (a dry run unless --yes)",
		Description: "Picks jobs that are done; --failed adds failed and died ones. Running jobs are never\n" +
			"touched. A job whose checkout has uncommitted changes, or commits that aren't on any\n" +
			"remote, is kept unless --force, so unpushed work isn't lost.",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "repo", Usage: "only this repo's jobs"},
			&cli.BoolFlag{Name: "failed", Usage: "also remove failed and died jobs"},
			&cli.BoolFlag{Name: "force", Usage: "remove picked jobs even with uncommitted or unpushed work"},
			&cli.BoolFlag{Name: "yes", Usage: "actually remove them"},
			&cli.StringFlag{Name: "workdir", Value: config.ExpandHome("~/prf-work"), Sources: cli.EnvVars("PRF_WORKDIR"), Usage: "where the job folders are (default: conf.json's workdir, else this)"},
		},
		Action: clean,
	}
}

func clean(ctx context.Context, c *cli.Command) error {
	states, err := readJobStates(flagOr(c, "workdir", cfg.Workdir), c.String("repo"))
	if err != nil {
		return cli.Exit("prf: "+err.Error(), exitUsage)
	}
	w := c.Root().Writer
	var picked int
	var freed int64
	for _, s := range states {
		switch s.State {
		case stateDone:
		case stateFailed, stateDied:
			if !c.Bool("failed") {
				continue
			}
		default:
			continue // setup or running
		}
		name := s.Repo + "/" + s.Job
		if reason := unsafeToRemove(s); reason != "" && !c.Bool("force") {
			_, _ = fmt.Fprintf(w, "keep    %-30s %s (--force removes it anyway)\n", name, reason)
			continue
		}
		size := dirSize(s.JobDir)
		picked++
		freed += size
		if !c.Bool("yes") {
			_, _ = fmt.Fprintf(w, "remove  %-30s %-6s %s\n", name, s.State, megabytes(size))
			continue
		}
		if err := os.RemoveAll(s.JobDir); err != nil {
			return cli.Exit("prf: "+err.Error(), exitSetup)
		}
		_, _ = fmt.Fprintf(w, "removed %-30s %-6s %s\n", name, s.State, megabytes(size))
	}
	switch {
	case picked == 0:
		_, _ = fmt.Fprintln(w, "nothing to remove")
	case c.Bool("yes"):
		_, _ = fmt.Fprintf(w, "freed %s\n", megabytes(freed))
	default:
		_, _ = fmt.Fprintf(w, "dry run: would free %s; add --yes to remove\n", megabytes(freed))
	}
	return nil
}

// unsafeToRemove says why removing a job would lose work, or "" if it
// wouldn't: its own clone has uncommitted changes or commits on no remote.
// A job that worked in your own checkout (apply) only loses its logs.
func unsafeToRemove(s jobState) string {
	if s.Dir != filepath.Join(s.JobDir, "repo") {
		return ""
	}
	if _, err := os.Stat(s.Dir); err != nil {
		return ""
	}
	if gitx.Output(s.Dir, "status", "--porcelain") != "" {
		return "uncommitted changes"
	}
	if n := gitx.Output(s.Dir, "rev-list", "--count", "HEAD", "--not", "--remotes"); n != "" && n != "0" {
		return n + " commits not on any remote"
	}
	return ""
}

func dirSize(dir string) int64 {
	var size int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				size += info.Size()
			}
		}
		return nil
	})
	if err != nil {
		logf("measuring %s: %v", dir, err)
	}
	return size
}

func megabytes(n int64) string {
	return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
}
