package main

import (
	"cmp"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/chrisrob11/prfoundry/internal/agent"
	"github.com/chrisrob11/prfoundry/internal/config"
	"github.com/chrisrob11/prfoundry/internal/gitx"
	"github.com/chrisrob11/prfoundry/internal/job"
	"github.com/chrisrob11/prfoundry/internal/state"
	"github.com/urfave/cli/v3"
)

func (a *app) resumeCmd() *cli.Command {
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
			agentFlag("(default: the agent the job ran)"),
			&cli.StringFlag{Name: "claude", Value: "claude", Sources: cli.EnvVars("CLAUDE_BIN"), Usage: "claude binary"},
			&cli.StringFlag{Name: "opencode", Value: "opencode", Sources: cli.EnvVars("OPENCODE_BIN"), Usage: "opencode binary"},
		},
		Action: a.resume,
	}
}

func (a *app) resume(ctx context.Context, c *cli.Command) error {
	if c.Args().Len() < 1 {
		return cli.Exit("prf: give the job to resume, as <repo>/<job>", job.ExitUsage)
	}
	workdir, err := filepath.Abs(flagOr(c, "workdir", a.cfg.Workdir))
	if err != nil {
		return cli.Exit("prf: "+err.Error(), job.ExitUsage)
	}
	st, err := state.Find(workdir, c.Args().First())
	if err != nil {
		return cli.Exit("prf: "+err.Error(), job.ExitUsage)
	}
	if st.Live() && state.ProcessAlive(st.PID) {
		return cli.Exit(fmt.Sprintf("prf: %s/%s is still running (process %d)", st.Repo, st.Job, st.PID), job.ExitUsage)
	}

	from := c.Int("from")
	if from == 0 {
		switch {
		case st.State == state.Done:
			return cli.Exit("prf: that job finished; use --from <step> to run steps again", job.ExitUsage)
		case st.Step == 0:
			return cli.Exit("prf: that job stopped during setup, before any step; start a new run", job.ExitUsage)
		}
		from = st.Step
	}
	if from < 1 || from > len(st.Roles) {
		return cli.Exit(fmt.Sprintf("prf: --from must be between 1 and %d", len(st.Roles)), job.ExitUsage)
	}
	if _, err := os.Stat(st.Dir); err != nil {
		return cli.Exit("prf: the job's checkout is gone: "+st.Dir, job.ExitUsage)
	}

	o, err := a.options(c, st.Repo, cmp.Or(st.Agent, agent.Claude))
	if err != nil {
		return cli.Exit("prf: "+err.Error(), job.ExitUsage)
	}
	o.Workdir, o.PR, o.Task, o.Pipeline = workdir, st.PR, st.Task, st.Pipeline
	o.AgentArgs = append(append([]string{}, a.agentArgs(o.Agent)...), c.Args().Tail()...)
	if !c.IsSet("push") && !c.IsSet("pr-create") {
		o.Push, o.PRCreate = st.Push, st.PRCreate // ship the way the job was started
	}
	return job.Resume(ctx, o, st, from)
}

func (a *app) cleanCmd() *cli.Command {
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
		Action: a.clean,
	}
}

func (a *app) clean(ctx context.Context, c *cli.Command) error {
	states, err := state.List(flagOr(c, "workdir", a.cfg.Workdir), c.String("repo"))
	if err != nil {
		return cli.Exit("prf: "+err.Error(), job.ExitUsage)
	}
	w := c.Root().Writer
	var picked int
	var freed int64
	for _, s := range states {
		switch s.State {
		case state.Done:
		case state.Failed, state.Died:
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
		size := dirSize(s.JobDir, a.logf)
		picked++
		freed += size
		if !c.Bool("yes") {
			_, _ = fmt.Fprintf(w, "remove  %-30s %-6s %s\n", name, s.State, megabytes(size))
			continue
		}
		if err := os.RemoveAll(s.JobDir); err != nil {
			return cli.Exit("prf: "+err.Error(), job.ExitSetup)
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
func unsafeToRemove(s state.Job) string {
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

func dirSize(dir string, logf func(format string, args ...any)) int64 {
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
