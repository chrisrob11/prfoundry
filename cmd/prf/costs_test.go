package main

import (
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCostsSumsEachRolesSpendAcrossJobs(t *testing.T) {
	e := newEnv(t, "")
	captureStderr(t)
	for _, branch := range []string{"one", "two"} {
		if code := e.run(t, "run", "--repo", e.origin, "--branch", branch, "--prompt", "x", "--role", "implement:opus,review:sonnet"); code != 0 {
			t.Fatalf("%s: exit %d", branch, code)
		}
	}
	s := readState(t, filepath.Join(e.root, "work", "origin", "one"))
	if len(s.Spend) != 2 || s.Spend[0] != (roleSpend{Role: "implement", Model: "opus", Runs: 1, Turns: 3, Cost: 0.05, CacheRead: 90000}) {
		t.Fatalf("one's spend = %+v", s.Spend)
	}

	code, out := runPrfIO(t, "", "--home", e.home, "costs", "--json")
	var rows []roleSpend
	if code != 0 || json.Unmarshal([]byte(out), &rows) != nil {
		t.Fatalf("costs --json = %d %s", code, out)
	}
	if len(rows) != 2 || rows[0].Runs != 2 || rows[0].Turns != 6 || math.Abs(rows[0].Cost-0.10) > 1e-9 || rows[0].CacheRead != 180000 {
		t.Errorf("rows = %+v, want each role summed over both jobs", rows)
	}

	_, text := runPrfIO(t, "", "--home", e.home, "costs")
	for _, want := range []string{"ROLE", "implement  opus", "review     sonnet", "50.0%", "30k", "total $0.20"} {
		if !strings.Contains(text, want) {
			t.Errorf("costs text missing %q:\n%s", want, text)
		}
	}
}

func TestSumSpendKeepsRecentJobsAndNamesUnrecordedCost(t *testing.T) {
	now := time.Now()
	states := []jobState{
		{Cost: 2, Updated: now, Spend: []roleSpend{{Role: "a", Model: "sonnet", Runs: 1, Cost: 0.5}, {Role: "b", Model: "opus", Runs: 1, Cost: 1.5}}},
		{Cost: 3, Updated: now.Add(-time.Hour)}, // from before spend was recorded
		{Cost: 9, Updated: now.Add(-72 * time.Hour), Spend: []roleSpend{{Role: "a", Model: "sonnet", Cost: 9}}}, // too old
	}
	rows, total, unrecorded := sumSpend(states, 48*time.Hour, now)
	if total != 5 || unrecorded != 3 || len(rows) != 2 || rows[0].Role != "b" || rows[1].Cost != 0.5 {
		t.Errorf("sumSpend = %+v total %v unrecorded %v", rows, total, unrecorded)
	}
	if rows, total, _ := sumSpend(states, 0, now); total != 14 || rows[0].Role != "a" || rows[0].Cost != 9.5 {
		t.Errorf("no --since = %+v total %v, want every job", rows, total)
	}
}
