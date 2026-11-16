package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/chrisrob11/prfoundry/internal/config"
	"github.com/chrisrob11/prfoundry/internal/dashboard"
	"github.com/chrisrob11/prfoundry/internal/job"
	"github.com/urfave/cli/v3"
)

func (a *app) uiCmd() *cli.Command {
	return &cli.Command{
		Name:  "ui",
		Usage: "serve a read-only dashboard of every job on localhost, and open it in your browser",
		Description: "Reads the job folders under the workdir, so it shows every job whoever started it.\n" +
			"The page is on 127.0.0.1 under a random token, kept in <home>/" + dashboard.LinkFile + " so a\n" +
			"bookmark keeps working. It runs until Ctrl-C.",
		Flags: []cli.Flag{
			&cli.IntFlag{Name: "port", Usage: "port to listen on (default: the last one used, if it's free)"},
			&cli.BoolFlag{Name: "no-open", Usage: "print the URL without opening a browser"},
			&cli.StringFlag{Name: "repo", Usage: "only this repo's jobs"},
			&cli.StringFlag{Name: "workdir", Value: config.ExpandHome("~/prf-work"), Sources: cli.EnvVars("PRF_WORKDIR"), Usage: "where the job folders are (default: conf.json's workdir, else this)"},
		},
		Action: a.ui,
	}
}

func (a *app) ui(ctx context.Context, c *cli.Command) error {
	srv, err := dashboard.Start(dashboard.Options{
		Workdir: flagOr(c, "workdir", a.cfg.Workdir),
		Repo:    c.String("repo"),
		Port:    c.Int("port"),
		Link:    filepath.Join(a.home, dashboard.LinkFile),
		Logf:    a.logf,
	})
	if err != nil {
		return cli.Exit("prf: "+err.Error(), job.ExitUsage)
	}
	defer srv.Close(ctx)
	_, _ = fmt.Fprintln(c.Root().Writer, srv.URL())
	if !c.Bool("no-open") {
		if err := a.rt.Browse(srv.URL()); err != nil {
			a.logf("opening the dashboard: %v", err)
		}
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	return nil
}
