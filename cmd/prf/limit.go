package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// limitRe matches Claude's usage-limit error, e.g. "You've hit your 5-hour
// limit · resets 12:30am (America/Los_Angeles)" or "You've hit your weekly
// limit · resets Nov 18, 9am (UTC)". Its groups: month, day, hour, minute,
// am/pm, zone; all but the limit itself are optional.
var limitRe = regexp.MustCompile(`(?i)hit your [^·]*limit(?:.*?resets (?:([a-z]{3}) (\d{1,2}),? )?(\d{1,2})(?::(\d{2}))?\s*([ap]m)(?: \(([^)]+)\))?)?`)

// limitFallback is how long to pause when the error names no reset time.
const limitFallback = 30 * time.Minute

// limitMargin is how long past the stated reset a paused step waits, so it
// doesn't start a moment early by a clock's difference.
const limitMargin = 5 * time.Minute

// pauseFor is how long to wait for reset; tests shorten it.
var pauseFor = func(reset time.Time) time.Duration { return time.Until(reset) + limitMargin }

// usageLimitReset reports whether res is Claude's usage-limit error and, if
// so, when the limit resets: the time it names, or limitFallback from now
// when it names none it can be read as.
func usageLimitReset(res phaseResult, now time.Time) (time.Time, bool) {
	if !res.isError {
		return time.Time{}, false
	}
	m := limitRe.FindStringSubmatch(res.text)
	if m == nil {
		return time.Time{}, false
	}
	if m[3] == "" {
		return now.Add(limitFallback), true
	}
	loc := now.Location()
	if m[6] != "" {
		if l, err := time.LoadLocation(m[6]); err == nil {
			loc = l
		}
	}
	hour, _ := strconv.Atoi(m[3])
	minute, _ := strconv.Atoi(m[4])
	if hour < 1 || hour > 12 || minute > 59 {
		return now.Add(limitFallback), true
	}
	hour %= 12
	if strings.EqualFold(m[5], "pm") {
		hour += 12
	}
	local := now.In(loc)
	if m[1] == "" {
		t := time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, loc)
		if t.Before(now.Add(-time.Minute)) {
			t = t.AddDate(0, 0, 1)
		}
		return t, true
	}
	month, err := time.Parse("Jan", strings.ToUpper(m[1][:1])+strings.ToLower(m[1][1:]))
	day, _ := strconv.Atoi(m[2])
	if err != nil || day < 1 || day > 31 {
		return now.Add(limitFallback), true
	}
	t := time.Date(local.Year(), month.Month(), day, hour, minute, 0, 0, loc)
	if t.Before(now.Add(-24 * time.Hour)) {
		t = t.AddDate(1, 0, 0)
	}
	return t, true
}

// pauseUntil marks the job paused until reset and waits it out, returning
// false if a signal stopped the wait.
func (j *job) pauseUntil(step, name string, reset time.Time) bool {
	wait := max(pauseFor(reset), 0)
	at := time.Now().Add(wait)
	j.event("PRF_PAUSE branch=%s phase=%s step=%s until=%s", j.branch, name, step, at.Format(time.RFC3339))
	logf("[%s] %s: hit the usage limit; paused until %s, then this step runs again", step, name, at.Format("Jan 2 3:04pm"))
	j.setState(func(s *jobState) { s.State, s.ResumeAt, s.Activity = statePaused, at, "" })
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		j.setState(func(s *jobState) { s.ResumeAt = time.Time{} })
		return true
	case sig := <-j.sigs:
		logf("[%s] %s: stopped while paused (%v)", step, name, sig)
		return false
	}
}

// pausedDetail is a paused job's status detail.
func pausedDetail(s jobState) string {
	return fmt.Sprintf("usage limit; step re-runs at %s", s.ResumeAt.Local().Format("Jan 2 3:04pm"))
}
