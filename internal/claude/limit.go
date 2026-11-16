package claude

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/chrisrob11/prfoundry/internal/agent"
)

// limitRe matches Claude's usage-limit error, e.g. "You've hit your 5-hour
// limit · resets 12:30am (America/Los_Angeles)" or "You've hit your weekly
// limit · resets Nov 18, 9am (UTC)". Its groups: month, day, hour, minute,
// am/pm, zone; all but the limit itself are optional.
var limitRe = regexp.MustCompile(`(?i)hit your [^·]*limit(?:.*?resets (?:([a-z]{3}) (\d{1,2}),? )?(\d{1,2})(?::(\d{2}))?\s*([ap]m)(?: \(([^)]+)\))?)?`)

// limitFallback is how long to pause when the error names no reset time.
const limitFallback = 30 * time.Minute

// limitReset reports whether r is Claude's usage-limit error and, if so,
// when the limit resets: the time it names, or limitFallback from now when
// it names none it can be read as.
func limitReset(r agent.Result, now time.Time) (time.Time, bool) {
	if !r.IsError {
		return time.Time{}, false
	}
	m := limitRe.FindStringSubmatch(r.Text)
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
