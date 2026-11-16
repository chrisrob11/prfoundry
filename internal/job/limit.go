package job

import (
	"time"

	"github.com/chrisrob11/prfoundry/internal/state"
)

// limitMargin is how long past the stated reset a paused step waits, so it
// doesn't start a moment early by a clock's difference.
const limitMargin = 5 * time.Minute

// pauseUntil marks the job paused until reset and waits it out, returning
// false if a signal stopped the wait.
func (j *Job) pauseUntil(step, name string, reset time.Time) bool {
	wait := max(j.rt.PauseFor(reset), 0)
	at := time.Now().Add(wait)
	j.event("PRF_PAUSE branch=%s phase=%s step=%s until=%s", j.branch, name, step, at.Format(time.RFC3339))
	j.logf("[%s] %s: hit the usage limit; paused until %s, then this step runs again", step, name, at.Format("Jan 2 3:04pm"))
	j.setState(func(s *state.Job) { s.State, s.ResumeAt, s.Activity = state.Paused, at, "" })
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		j.setState(func(s *state.Job) { s.ResumeAt = time.Time{} })
		return true
	case sig := <-j.sigs:
		j.logf("[%s] %s: stopped while paused (%v)", step, name, sig)
		return false
	}
}
