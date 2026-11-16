package main

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/chrisrob11/prfoundry/internal/config"
	"github.com/chrisrob11/prfoundry/internal/human"
	"github.com/chrisrob11/prfoundry/internal/job"
	"github.com/chrisrob11/prfoundry/internal/jsonio"
	"github.com/chrisrob11/prfoundry/internal/state"
	"github.com/urfave/cli/v3"
)

func (a *app) statusCmd() *cli.Command {
	return &cli.Command{
		Name:  "status",
		Usage: "show every job under the workdir: its state, pipeline step, and what Claude is doing",
		Description: "Reads <workdir>/<repo>/<job>/state.json, which each job rewrites as it moves.\n" +
			"States: setup, running, review (waiting for your review at a @review gate), ask (waiting for\n" +
			"your answers at an @ask gate), paused (the usage limit stopped a step; it re-runs after the reset),\n" +
			"done, failed, and died (says it's running, but its process is gone).",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "repo", Usage: "only this repo's jobs"},
			&cli.StringFlag{Name: "workdir", Value: config.ExpandHome("~/prf-work"), Sources: cli.EnvVars("PRF_WORKDIR"), Usage: "where the job folders are (default: conf.json's workdir, else this)"},
			&cli.BoolFlag{Name: "json", Usage: "print JSON"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			states, err := state.List(flagOr(c, "workdir", a.cfg.Workdir), c.String("repo"))
			if err != nil {
				return cli.Exit("prf: "+err.Error(), job.ExitUsage)
			}
			w := c.Root().Writer
			if c.Bool("json") {
				if states == nil {
					states = []state.Job{}
				}
				return jsonio.Write(w, states)
			}
			if len(states) == 0 {
				_, _ = fmt.Fprintln(w, "no jobs")
				return nil
			}
			tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "REPO\tJOB\tSTATE\tSTEP\tSTAGE\tTIME\tDETAIL")
			for _, s := range states {
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.Repo, s.Job, s.State, stepOf(s), stageOf(s), timeOf(s), detailOf(s))
			}
			return tw.Flush()
		},
	}
}

// stageOf is the stage, with the running role's model if it has one.
func stageOf(s state.Job) string {
	if s.State == state.Ask {
		return "ask (waiting for owner)"
	}
	if s.Step > 0 && s.Step <= len(s.Models) && s.Models[s.Step-1] != "" {
		return fmt.Sprintf("%s (%s)", s.Stage, s.Models[s.Step-1])
	}
	return s.Stage
}

func stepOf(s state.Job) string {
	if s.Step == 0 {
		return "-"
	}
	return fmt.Sprintf("%d/%d", s.Step, len(s.Roles))
}

// timeOf is how long a live job has run, or how long ago a finished one ended.
func timeOf(s state.Job) string {
	if s.Live() {
		return human.Duration(time.Since(s.Started))
	}
	return human.Duration(time.Since(s.Updated)) + " ago"
}

func detailOf(s state.Job) string {
	switch s.State {
	case state.Setup, state.Running:
		return s.Activity
	case state.Review:
		return "waiting for your review: " + s.ReviewURL
	case state.Ask:
		return s.ReviewURL
	case state.Paused:
		return pausedDetail(s)
	case state.Died:
		return fmt.Sprintf("process %d is gone", s.PID)
	}
	var parts []string
	if s.PRURL != "" {
		parts = append(parts, s.PRURL)
	} else if s.Pushed {
		parts = append(parts, "pushed")
	}
	if s.Exit != nil {
		parts = append(parts, fmt.Sprintf("exit %d", *s.Exit))
	}
	if s.Cost > 0 {
		parts = append(parts, fmt.Sprintf("$%.2f", s.Cost))
	}
	return strings.Join(parts, ", ")
}

// pausedDetail is a paused job's status detail.
func pausedDetail(s state.Job) string {
	return fmt.Sprintf("usage limit; step re-runs at %s", s.ResumeAt.Local().Format("Jan 2 3:04pm"))
}
