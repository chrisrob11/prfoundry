package main

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/chrisrob11/prfoundry/internal/codeindex"
	"github.com/chrisrob11/prfoundry/internal/gitx"
	"github.com/chrisrob11/prfoundry/internal/job"
	"github.com/urfave/cli/v3"
)

func indexCmd(ctx context.Context, c *cli.Command) error {
	dir, err := filepath.Abs(c.String("dir"))
	if err != nil {
		return cli.Exit("prf: "+err.Error(), job.ExitUsage)
	}
	start := time.Now()
	if err := codeindex.Build(dir, filepath.Base(dir), firstNonEmpty(gitx.Output(dir, "branch", "--show-current"), "HEAD"), c.String("out")); err != nil {
		return cli.Exit("prf: "+err.Error(), job.ExitSetup)
	}
	_, _ = fmt.Fprintf(c.Root().Writer, "wrote %s in %s\n", filepath.Join(c.String("out"), "index.md"), time.Since(start).Round(time.Millisecond))
	return nil
}
