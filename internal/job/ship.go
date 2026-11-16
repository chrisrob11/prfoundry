package job

import (
	"github.com/chrisrob11/prfoundry/internal/config"
	"github.com/chrisrob11/prfoundry/internal/human"
	"github.com/chrisrob11/prfoundry/internal/ship"
	"github.com/chrisrob11/prfoundry/internal/state"
)

// ship pushes the branch and opens its PR, when asked to, once every step
// has passed (see internal/ship). It returns 0, or ExitShip if pushing or
// opening the PR failed.
func (j *Job) ship() int {
	if !j.push {
		return 0
	}
	j.setState(func(s *state.Job) { s.Stage, s.Activity = "pushing", "" })
	j.logf("pushing %s", j.branch)
	out, err := ship.Push(j.dir, j.branch)
	j.event("PRF_PUSH branch=%s exit=%d", j.branch, ship.ExitCode(err))
	if err != nil {
		j.logf("push failed, so no PR either: %s", human.OneLine(out, 300))
		return ExitShip
	}
	j.setState(func(s *state.Job) { s.Pushed = true })
	if j.prCreate == "" {
		return 0
	}

	j.setState(func(s *state.Job) { s.Stage = "opening PR" })
	url, existed, err := ship.Open(ship.PR{
		Dir: j.dir, Branch: j.branch, Base: j.base, Draft: j.prCreate == config.PRDraft,
		TitleFile: j.prTitleFile, BodyFile: j.prBodyFile,
	})
	if existed {
		j.logf("a PR for %s is already open", j.branch)
	}
	if err != nil {
		j.logf("pushed, but no PR: %v", err)
		j.event("PRF_PR branch=%s exit=1", j.branch)
		return ExitShip
	}
	j.event("PRF_PR branch=%s url=%s", j.branch, url)
	j.setState(func(s *state.Job) { s.PRURL = url })
	j.logf("PR: %s", url)
	return 0
}
