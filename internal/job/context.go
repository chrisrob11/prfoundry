package job

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"

	"github.com/chrisrob11/prfoundry/internal/codeindex"
	"github.com/chrisrob11/prfoundry/internal/pipeline"
)

// prepareContext fills <job>/context before the first phase: the built-in
// codebase map (unless the repo's pipeline.json sets "index": false), then
// whatever the repo's "setup" commands write there. It records what's at the
// top of the folder, with a trailing / for directories, so every phase's
// prompt can point at it without listing every package file.
func (j *Job) prepareContext() error {
	p, _, err := pipeline.Read(j.home, j.name)
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
		j.logf("building codebase map")
		j.setupStage("building codebase map")
		if err := codeindex.Build(j.dir, j.name, j.branch, j.contextDir); err != nil {
			j.logf("warning: no codebase map: %v", err)
		}
	}
	for _, line := range p.Setup {
		j.logf("setup: %s", line)
		j.setupStage("running " + line)
		cmd := exec.Command("sh", "-c", line)
		cmd.Dir = j.dir
		cmd.Env = append(slices.Clip(j.env), "PRF_CONTEXT="+j.contextDir, "PRF_JOB="+j.jobDir)
		cmd.Stdout, cmd.Stderr = j.stderr, j.stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("setup command %q: %w", line, err)
		}
	}

	return j.listContext()
}

// listContext records what's at the top of the context folder, with a
// trailing / for directories, so every prompt can point at it.
func (j *Job) listContext() error {
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
