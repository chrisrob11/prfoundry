package main

import (
	"context"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/chrisrob11/prfoundry/internal/config"
	"github.com/chrisrob11/prfoundry/internal/human"
	"github.com/chrisrob11/prfoundry/internal/jsonio"
	"github.com/chrisrob11/prfoundry/internal/state"
	"github.com/urfave/cli/v3"
)

func (a *app) costsCmd() *cli.Command {
	return &cli.Command{
		Name:  "costs",
		Usage: "sum what each role has cost across jobs, by role and model, most expensive first",
		Description: "Reads each job's state.json spend. CTX/TURN is the context Claude re-read per turn, in\n" +
			"thousands of tokens: a role with many turns at a big context is where a long pipeline's money goes.\n" +
			"Jobs that ran before prf recorded spend count in the total only.",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "repo", Usage: "only this repo's jobs"},
			&cli.StringFlag{Name: "workdir", Value: config.ExpandHome("~/prf-work"), Sources: cli.EnvVars("PRF_WORKDIR"), Usage: "where the job folders are (default: conf.json's workdir, else this)"},
			&cli.DurationFlag{Name: "since", Usage: "only jobs active in this long, e.g. 48h (default: every job)"},
			&cli.BoolFlag{Name: "json", Usage: "print JSON"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			states, err := state.List(flagOr(c, "workdir", a.cfg.Workdir), c.String("repo"))
			if err != nil {
				return cli.Exit("prf: "+err.Error(), exitUsage)
			}
			rows, total, unrecorded := state.Sum(states, c.Duration("since"), time.Now())
			w := c.Root().Writer
			if c.Bool("json") {
				if rows == nil {
					rows = []state.Spend{}
				}
				return jsonio.Write(w, rows)
			}
			if total == 0 {
				_, _ = fmt.Fprintln(w, "no spend recorded")
				return nil
			}
			if len(rows) == 0 {
				_, _ = fmt.Fprintf(w, "no per-role spend recorded; total %s, all in jobs that ran before prf recorded it\n", human.USD(total))
				return nil
			}
			tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "ROLE\tMODEL\tCOST\tSHARE\tRUNS\tTURNS/RUN\tCTX/TURN")
			for _, r := range rows {
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%.1f%%\t%d\t%.1f\t%s\n", r.Role, firstNonEmpty(r.Model, "-"), human.USD(r.Cost),
					100*r.Cost/total, r.Runs, float64(r.Turns)/float64(max(1, r.Runs)), perTurn(r))
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(w, "total %s", human.USD(total))
			if unrecorded > 0 {
				_, _ = fmt.Fprintf(w, ", %s of it in jobs with no per-role spend", human.USD(unrecorded))
			}
			_, _ = fmt.Fprintln(w)
			return nil
		},
	}
}

// perTurn is the context re-read per turn, in thousands of tokens.
func perTurn(r state.Spend) string {
	if r.Turns == 0 || r.CacheRead == 0 {
		return "-"
	}
	return fmt.Sprintf("%dk", r.CacheRead/int64(r.Turns)/1000)
}
