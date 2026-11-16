package claude

import (
	"testing"
	"time"

	"github.com/chrisrob11/prfoundry/internal/agent"
)

func TestLimitResetReadsTheStatedTime(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skip(err)
	}
	now := time.Date(2026, 11, 15, 0, 10, 0, 0, la) // 12:10am
	cases := []struct {
		text string
		want time.Time
	}{
		{"You've hit your 5-hour limit · resets 12:30am (America/Los_Angeles)", time.Date(2026, 11, 15, 0, 30, 0, 0, la)},
		{"You've hit your 5-hour limit · resets 3pm (America/Los_Angeles)", time.Date(2026, 11, 15, 15, 0, 0, 0, la)},
		{"You've hit your 5-hour limit · resets 12am (America/Los_Angeles)", time.Date(2026, 11, 16, 0, 0, 0, 0, la)},        // already past today: tomorrow
		{"You've hit your 5-hour limit · resets 9:00am (UTC)", time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)},                // 9am UTC is 1am here, still ahead
		{"You've hit your weekly limit · resets Nov 18, 9am (America/Los_Angeles)", time.Date(2026, 11, 18, 9, 0, 0, 0, la)}, // a dated reset
		{"You've hit your weekly limit · resets Jan 2, 9am (America/Los_Angeles)", time.Date(2027, 1, 2, 9, 0, 0, 0, la)},    // next year
		{"You've hit your Opus limit", now.Add(limitFallback)},                                                               // no time named
		{"You've hit your 5-hour limit · resets 13:70pm (UTC)", now.Add(limitFallback)},                                      // unreadable time
	}
	for _, c := range cases {
		got, ok := limitReset(agent.Result{IsError: true, Text: c.text}, now)
		if !ok || !got.Equal(c.want) {
			t.Errorf("%q: got %v %t, want %v", c.text, got, ok, c.want)
		}
	}
}

func TestLimitResetIgnoresOtherResults(t *testing.T) {
	now := time.Now()
	for _, res := range []agent.Result{
		{IsError: true, Text: "it broke"},
		{IsError: false, Text: "You've hit your 5-hour limit · resets 3pm"}, // a success that quotes the message
		{},
	} {
		if _, ok := limitReset(res, now); ok {
			t.Errorf("%+v read as a usage limit", res)
		}
	}
}
