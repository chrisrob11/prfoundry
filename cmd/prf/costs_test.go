package main

import (
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chrisrob11/prfoundry/internal/state"
)

func TestCostsSumsEachRolesSpendAcrossJobs(t *testing.T) {
	e := newEnv(t, "")
	e.captureStderr()
	for _, branch := range []string{"one", "two"} {
		if code := e.run(t, "run", "--repo", e.origin, "--branch", branch, "--prompt", "x", "--role", "implement:opus,review:sonnet"); code != 0 {
			t.Fatalf("%s: exit %d", branch, code)
		}
	}
	s := readState(t, filepath.Join(e.root, "work", "origin", "one"))
	if len(s.Spend) != 2 || s.Spend[0] != (state.Spend{Role: "implement", Model: "opus", Runs: 1, Turns: 3, Cost: 0.05, CacheRead: 90000}) {
		t.Fatalf("one's spend = %+v", s.Spend)
	}

	code, out := e.runIO(t, "costs", "--json")
	var rows []state.Spend
	if code != 0 || json.Unmarshal([]byte(out), &rows) != nil {
		t.Fatalf("costs --json = %d %s", code, out)
	}
	if len(rows) != 2 || rows[0].Runs != 2 || rows[0].Turns != 6 || math.Abs(rows[0].Cost-0.10) > 1e-9 || rows[0].CacheRead != 180000 {
		t.Errorf("rows = %+v, want each role summed over both jobs", rows)
	}

	_, text := e.runIO(t, "costs")
	for _, want := range []string{"ROLE", "implement  opus", "review     sonnet", "50.0%", "30k", "total $0.20"} {
		if !strings.Contains(text, want) {
			t.Errorf("costs text missing %q:\n%s", want, text)
		}
	}
}
