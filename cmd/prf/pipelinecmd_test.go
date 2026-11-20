package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/chrisrob11/prfoundry/internal/job"
	"github.com/chrisrob11/prfoundry/internal/pipeline"
)

// An @ask gate must follow a role, in pipeline set and in run --role.
func TestAskPipelineValidation(t *testing.T) {
	e := newEnv(t, "")
	e.captureStderr()
	for _, roles := range []string{"@ask,implement", "implement,@review,@ask"} {
		if code := e.run(t, "pipeline", "set", "p", "--repo", "origin", "--roles", roles); code != job.ExitUsage {
			t.Errorf("pipeline set %s: exit %d, want %d", roles, code, job.ExitUsage)
		}
	}
	if code := e.run(t, "run", "--repo", e.origin, "--branch", "b", "--prompt", "x", "--role", "@ask"); code != job.ExitUsage {
		t.Errorf("run --role @ask: exit %d, want %d", code, job.ExitUsage)
	}
}

// roleSteps makes steps with no model.
func roleSteps(names ...string) []pipeline.Step {
	steps := make([]pipeline.Step, len(names))
	for i, n := range names {
		steps[i] = pipeline.Step{Role: n}
	}
	return steps
}

func TestPipelineCommand(t *testing.T) {
	h := newHome(t)
	path := filepath.Join(h, "app", pipeline.FileName)
	pipe := func(stdin string, args ...string) (int, string) {
		return prf(t, h, stdin, append([]string{"pipeline"}, args...)...)
	}
	saved := func() pipeline.File {
		t.Helper()
		var p pipeline.File
		b, err := os.ReadFile(path)
		if err != nil || json.Unmarshal(b, &p) != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		return p
	}

	// With no pipeline.json, list shows the built-in default and writes nothing.
	if code, out := pipe("", "list", "--repo", "app"); code != 0 || !strings.Contains(out, "* default      implement -> test -> review") {
		t.Errorf("list without pipeline.json = %d %q", code, out)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("list created pipeline.json")
	}

	if code, _ := pipe("implement\nperf\n", "set", "feature", "--repo", "app"); code != job.ExitUsage {
		t.Fatalf("set with a missing role: exit %d", code)
	}
	writeRole(t, filepath.Join(h, "app", "roles"), "perf", "perf on {{.Branch}}")
	if code, _ := pipe("implement\nperf\ntest\nreview\n", "set", "feature", "--repo", "app"); code != 0 {
		t.Fatalf("set feature: exit %d", code)
	}
	if code, _ := pipe("", "set", "bugfix", "--repo", "app", "--roles", "implement,test"); code != 0 {
		t.Fatalf("set bugfix: exit %d", code)
	}
	want := pipeline.File{Default: "feature", Pipelines: map[string][]pipeline.Step{
		"feature": roleSteps("implement", "perf", "test", "review"),
		"bugfix":  roleSteps("implement", "test"),
	}}
	if got := saved(); !reflect.DeepEqual(got, want) {
		t.Errorf("pipeline.json = %+v, want %+v (first set becomes the default, built-in not persisted)", got, want)
	}

	if code, out := pipe("", "show", "bugfix", "--repo", "app"); code != 0 || out != "implement\ntest\n" {
		t.Errorf("show = %d %q", code, out)
	}
	if code, _ := pipe("", "default", "bugfix", "--repo", "app"); code != 0 || saved().Default != "bugfix" {
		t.Errorf("default: exit %d, default %q", code, saved().Default)
	}
	if code, _ := pipe("", "default", "nope", "--repo", "app"); code != job.ExitUsage {
		t.Errorf("default of a missing pipeline: exit %d", code)
	}

	code, out := pipe("", "list", "--repo", "app", "--json")
	var entries []pipelineEntry
	if code != 0 || json.Unmarshal([]byte(out), &entries) != nil || len(entries) != 2 || entries[0].Name != "bugfix" || !entries[0].Default || !entries[1].Valid {
		t.Errorf("list --json = %d %s", code, out)
	}

	// Removing a role a pipeline uses shows up as an invalid pipeline.
	if code, _ := prf(t, h, "", "role", "remove", "perf", "--repo", "app"); code != 0 {
		t.Fatal("role remove failed")
	}
	if _, text := pipe("", "list", "--repo", "app"); !strings.Contains(text, "feature") || !strings.Contains(text, `INVALID: no role "perf"`) {
		t.Errorf("list should flag feature as invalid:\n%s", text)
	}

	if code, _ := pipe("", "remove", "bugfix", "--repo", "app"); code != 0 || saved().Default != "" {
		t.Errorf("remove default: exit %d, default %q", code, saved().Default)
	}
	if code, _ := pipe("", "remove", "bugfix", "--repo", "app"); code != job.ExitUsage {
		t.Errorf("removing a missing pipeline: exit %d", code)
	}
	if code, _ := pipe("", "list"); code == 0 {
		t.Error("list without --repo should fail")
	}
}
