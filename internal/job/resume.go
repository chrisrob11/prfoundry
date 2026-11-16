package job

import (
	"cmp"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/chrisrob11/prfoundry/internal/agent"
	"github.com/chrisrob11/prfoundry/internal/mise"
	"github.com/chrisrob11/prfoundry/internal/pipeline"
	"github.com/chrisrob11/prfoundry/internal/roles"
	"github.com/chrisrob11/prfoundry/internal/state"
)

// Resume carries on the job st, which has stopped, from step from
// (counting from 1), in its checkout: same branch, PR spec, prompt, pipeline
// and models, and agent unless o names another (the models are then the
// steps' own for it), with the current role files and o's other settings. The
// setup isn't repeated, and steps keep their numbers. o's Steps, Out and
// Status aren't used: the steps are st's, and the log and status file stay
// in the job folder. Its error is the job's outcome, with prf's exit code.
func Resume(o Options, st state.Job, from int) error {
	// An ask gate re-runs the step before it, so a resume there loads that
	// step too, then drops it.
	first := from - 1
	if len(st.Steps) == len(st.Roles) && st.Steps[first].Gate == pipeline.GateAsk && first > 0 {
		first--
	}
	agentName := o.Agent.Name()
	sameAgent := cmp.Or(st.Agent, agent.Claude) == agentName
	steps := make([]pipeline.Step, 0, len(st.Roles)-first)
	for i := first; i < len(st.Roles); i++ {
		s, _ := pipeline.ParseStep(st.Roles[i])
		if len(st.Steps) == len(st.Roles) {
			s = st.Steps[i] // keeps a gate's settings
		}
		s = stepFor(s, agentName)
		if sameAgent && i < len(st.Models) {
			s.Model = st.Models[i]
		}
		steps = append(steps, s)
	}
	j := newJob(o)
	j.jobDir, j.dir, j.branch = st.JobDir, st.Dir, st.Branch
	j.out, j.tee = filepath.Join(st.JobDir, "claude.log"), true
	j.status = filepath.Join(st.JobDir, "status")
	j.contextDir = filepath.Join(st.JobDir, "context")
	j.firstStep, j.totalSteps, j.priorCost = from-1, len(st.Roles), st.Cost
	if err := j.useSandbox(); err != nil {
		return exitError{"prf: " + err.Error(), ExitSetup}
	}
	j.env = mise.Env(mise.TrustedDir(j.dir, j.workdir))
	if err := j.listContext(); err != nil && !errors.Is(err, fs.ErrNotExist) { // a missing one just means no context
		j.logf("warning: listing the context folder: %v", err)
	}
	phases, err := loadPhases(steps, roles.Dirs(j.home, j.name), j.pr != "" || j.task != "", j.task != "")
	if err != nil {
		return exitError{"prf: " + err.Error(), ExitUsage}
	}
	phases, j.roles = phases[from-1-first:], steps[from-1-first:]
	if err := pipeline.CheckCommands(j.roles, j.checkCmd); err != nil {
		return exitError{"prf: " + err.Error(), ExitUsage}
	}

	st.PID, st.Exit, st.Activity = os.Getpid(), nil, ""
	j.state = st
	j.logf("resuming %s/%s from step %d/%d (%s)", st.Repo, st.Job, from, len(st.Roles), st.Roles[from-1])
	return j.runPhases(phases)
}
