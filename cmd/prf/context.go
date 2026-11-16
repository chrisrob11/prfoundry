package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"time"

	"github.com/chrisrob11/prfoundry/internal/codeindex"
	"github.com/chrisrob11/prfoundry/internal/gitx"
	"github.com/chrisrob11/prfoundry/internal/pipeline"
	"github.com/urfave/cli/v3"
)

func indexCmd(ctx context.Context, c *cli.Command) error {
	dir, err := filepath.Abs(c.String("dir"))
	if err != nil {
		return cli.Exit("prf: "+err.Error(), exitUsage)
	}
	start := time.Now()
	if err := codeindex.Build(dir, filepath.Base(dir), firstNonEmpty(gitx.Output(dir, "branch", "--show-current"), "HEAD"), c.String("out")); err != nil {
		return cli.Exit("prf: "+err.Error(), exitSetup)
	}
	_, _ = fmt.Fprintf(c.Root().Writer, "wrote %s in %s\n", filepath.Join(c.String("out"), "index.md"), time.Since(start).Round(time.Millisecond))
	return nil
}

// prepareContext fills <job>/context before the first phase: the built-in
// codebase map (unless the repo's pipeline.json sets "index": false), then
// whatever the repo's "setup" commands write there. It records what's at the
// top of the folder, with a trailing / for directories, so every phase's
// prompt can point at it without listing every package file.
func (j *job) prepareContext() error {
	p, _, err := pipeline.Read(home, j.name)
	if err != nil {
		return err
	}
	j.contextDir = filepath.Join(j.jobDir, "context")
	if err := os.RemoveAll(j.contextDir); err != nil {
		return err
	}
	if err := os.MkdirAll(j.contextDir, 0o755); err != nil {
		return err
	}

	if p.Index == nil || *p.Index {
		logf("building codebase map")
		j.setupStage("building codebase map")
		if err := codeindex.Build(j.dir, j.name, j.branch, j.contextDir); err != nil {
			logf("warning: no codebase map: %v", err)
		}
	}
	for _, line := range p.Setup {
		logf("setup: %s", line)
		j.setupStage("running " + line)
		cmd := exec.Command("sh", "-c", line)
		cmd.Dir = j.dir
		cmd.Env = append(slices.Clip(j.env), "PRF_CONTEXT="+j.contextDir, "PRF_JOB="+j.jobDir)
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("setup command %q: %w", line, err)
		}
	}

	return j.listContext()
}

// listContext records what's at the top of the context folder, with a
// trailing / for directories, so every prompt can point at it.
func (j *job) listContext() error {
	entries, err := os.ReadDir(j.contextDir)
	if err != nil {
		return err
	}
	j.contextFiles = nil
	for _, e := range entries {
		p := filepath.Join(j.contextDir, e.Name())
		if e.IsDir() {
			p += string(filepath.Separator)
		}
		j.contextFiles = append(j.contextFiles, p)
	}
	return nil
}
