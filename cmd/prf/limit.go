package main

import (
	"fmt"
	"time"

	"github.com/chrisrob11/prfoundry/internal/state"
)

// limitMargin is how long past the stated reset a paused step waits, so it
// doesn't start a moment early by a clock's difference.
const limitMargin = 5 * time.Minute

// pauseFor is how long to wait for reset; tests shorten it.
var pauseFor = func(reset time.Time) time.Duration { return time.Until(reset) + limitMargin }

// pauseUntil marks the job paused until reset and waits it out, returning
// false if a signal stopped the wait.
func (j *job) pauseUntil(step, name string, reset time.Time) bool {
	wait := max(pauseFor(reset), 0)
	at := time.Now().Add(wait)
	j.event("PRF_PAUSE branch=%s phase=%s step=%s until=%s", j.branch, name, step, at.Format(time.RFC3339))
	logf("[%s] %s: hit the usage limit; paused until %s, then this step runs again", step, name, at.Format("Jan 2 3:04pm"))
	j.setState(func(s *state.Job) { s.State, s.ResumeAt, s.Activity = state.Paused, at, "" })
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		j.setState(func(s *state.Job) { s.ResumeAt = time.Time{} })
		return true
	case sig := <-j.sigs:
		logf("[%s] %s: stopped while paused (%v)", step, name, sig)
		return false
	}
}

// pausedDetail is a paused job's status detail.
func pausedDetail(s state.Job) string {
	return fmt.Sprintf("usage limit; step re-runs at %s", s.ResumeAt.Local().Format("Jan 2 3:04pm"))
}
