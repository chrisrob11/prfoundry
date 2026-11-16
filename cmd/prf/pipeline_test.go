package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// roles makes steps with no model.
func roles(names ...string) []step {
	steps := make([]step, len(names))
	for i, n := range names {
		steps[i] = step{Role: n}
	}
	return steps
}

func TestParseRoleList(t *testing.T) {
	got, err := parseRoleList("# header\nimplement:opus\n\n  perf-test  # benchmarks\ntest, review : haiku\n")
	want := []step{{Role: "implement", Model: "opus"}, {Role: "perf-test"}, {Role: "test"}, {Role: "review", Model: "haiku"}}
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("got %v, %v; want %v", got, err, want)
	}
	for _, bad := range []string{"implement:", "implement:bad model", "test:x/y"} {
		if _, err := parseRoleList(bad); err == nil {
			t.Errorf("parseRoleList(%q) should fail", bad)
		}
	}
}

func TestStepJSON(t *testing.T) {
	var steps []step
	if err := json.Unmarshal([]byte(`["implement", {"role": "review", "model": "haiku"}, {"role": "test", "max_cost": 2.5}]`), &steps); err != nil {
		t.Fatal(err)
	}
	if want := []step{{Role: "implement"}, {Role: "review", Model: "haiku"}, {Role: "test", MaxCost: 2.5}}; !slices.Equal(steps, want) {
		t.Errorf("decoded %v, want %v", steps, want)
	}
	b, _ := json.Marshal(steps)
	if want := `["implement",{"role":"review","model":"haiku"},{"role":"test","max_cost":2.5}]`; string(b) != want {
		t.Errorf("encoded %s, want %s (a step without a model stays a plain string)", b, want)
	}
	for _, bad := range []string{`[{"role": "x", "modle": "y"}]`, `[5]`} { //nolint:misspell // deliberate typo: tests unknown-field rejection
		if json.Unmarshal([]byte(bad), &steps) == nil {
			t.Errorf("%s should not decode", bad)
		}
	}
}

func TestPipelineRoles(t *testing.T) {
	home = t.TempDir()
	writeFileT(t, filepath.Join(home, "app", pipelineFile), `{"default": "feature", "pipelines": {"feature": ["implement", "review"], "empty": []}}`)
	writeFileT(t, filepath.Join(home, "nodefault", pipelineFile), `{"pipelines": {"x": ["review"]}}`)
	writeFileT(t, filepath.Join(home, "typo", pipelineFile), `{"defualt": "x", "pipelines": {}}`) //nolint:misspell // deliberate typo: tests unknown-field rejection

	tests := []struct {
		repo, name string
		want       []step
		wantErr    string
	}{
		{repo: "app", want: roles("implement", "review")},
		{repo: "app", name: "feature", want: roles("implement", "review")},
		{repo: "app", name: "nope", wantErr: `no pipeline "nope" (it has: empty, feature)`},
		{repo: "app", name: "empty", wantErr: "lists no roles"},
		{repo: "other", want: builtinPipeline},
		{repo: "nodefault", wantErr: "no default pipeline"},
		{repo: "typo", wantErr: `unknown field "defualt"`}, //nolint:misspell // deliberate typo: tests unknown-field rejection
	}
	for _, tt := range tests {
		t.Run(tt.repo+"/"+tt.name, func(t *testing.T) {
			got, _, err := pipelineRoles(tt.repo, tt.name)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPipelineCommand(t *testing.T) {
	h := newHome(t)
	path := filepath.Join(h, "app", pipelineFile)
	pipe := func(stdin string, args ...string) (int, string) {
		return prf(t, h, stdin, append([]string{"pipeline"}, args...)...)
	}
	saved := func() pipelines {
		t.Helper()
		var p pipelines
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

	if code, _ := pipe("implement\nperf\n", "set", "feature", "--repo", "app"); code != exitUsage {
		t.Fatalf("set with a missing role: exit %d", code)
	}
	writeRole(t, filepath.Join(h, "app", "roles"), "perf", "perf on {{.Branch}}")
	if code, _ := pipe("implement\nperf\ntest\nreview\n", "set", "feature", "--repo", "app"); code != 0 {
		t.Fatalf("set feature: exit %d", code)
	}
	if code, _ := pipe("", "set", "bugfix", "--repo", "app", "--roles", "implement,test"); code != 0 {
		t.Fatalf("set bugfix: exit %d", code)
	}
	want := pipelines{Default: "feature", Pipelines: map[string][]step{
		"feature": roles("implement", "perf", "test", "review"),
		"bugfix":  roles("implement", "test"),
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
	if code, _ := pipe("", "default", "nope", "--repo", "app"); code != exitUsage {
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
	if code, _ := pipe("", "remove", "bugfix", "--repo", "app"); code != exitUsage {
		t.Errorf("removing a missing pipeline: exit %d", code)
	}
	if code, _ := pipe("", "list"); code == 0 {
		t.Error("list without --repo should fail")
	}
}
