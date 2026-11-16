package pipeline

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/chrisrob11/prfoundry/internal/atomicfile"
)

// roleSteps makes steps with no model.
func roleSteps(names ...string) []Step {
	s := make([]Step, len(names))
	for i, n := range names {
		s[i] = Step{Role: n}
	}
	return s
}

func writeFileT(t *testing.T, path, s string) {
	t.Helper()
	if err := atomicfile.Write(path, []byte(s)); err != nil {
		t.Fatal(err)
	}
}

func TestParseSteps(t *testing.T) {
	got, err := ParseSteps("# header\nimplement:opus\n\n  perf-test  # benchmarks\ntest, review : haiku\n")
	want := []Step{{Role: "implement", Model: "opus"}, {Role: "perf-test"}, {Role: "test"}, {Role: "review", Model: "haiku"}}
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("got %v, %v; want %v", got, err, want)
	}
	for _, bad := range []string{"implement:", "implement:bad model", "test:x/y"} {
		if _, err := ParseSteps(bad); err == nil {
			t.Errorf("ParseSteps(%q) should fail", bad)
		}
	}
}

func TestStepJSON(t *testing.T) {
	var steps []Step
	if err := json.Unmarshal([]byte(`["implement", {"role": "review", "model": "haiku"}, {"role": "test", "max_cost": 2.5}]`), &steps); err != nil {
		t.Fatal(err)
	}
	if want := []Step{{Role: "implement"}, {Role: "review", Model: "haiku"}, {Role: "test", MaxCost: 2.5}}; !slices.Equal(steps, want) {
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

func TestLookup(t *testing.T) {
	home := t.TempDir()
	writeFileT(t, filepath.Join(home, "app", FileName), `{"default": "feature", "pipelines": {"feature": ["implement", "review"], "empty": []}}`)
	writeFileT(t, filepath.Join(home, "nodefault", FileName), `{"pipelines": {"x": ["review"]}}`)
	writeFileT(t, filepath.Join(home, "typo", FileName), `{"defualt": "x", "pipelines": {}}`) //nolint:misspell // deliberate typo: tests unknown-field rejection

	tests := []struct {
		repo, name string
		want       []Step
		wantErr    string
	}{
		{repo: "app", want: roleSteps("implement", "review")},
		{repo: "app", name: "feature", want: roleSteps("implement", "review")},
		{repo: "app", name: "nope", wantErr: `no pipeline "nope" (it has: empty, feature)`},
		{repo: "app", name: "empty", wantErr: "lists no roles"},
		{repo: "other", want: Builtin()},
		{repo: "nodefault", wantErr: "no default pipeline"},
		{repo: "typo", wantErr: `unknown field "defualt"`}, //nolint:misspell // deliberate typo: tests unknown-field rejection
	}
	for _, tt := range tests {
		t.Run(tt.repo+"/"+tt.name, func(t *testing.T) {
			got, _, err := Lookup(home, tt.repo, tt.name)
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

func TestGateSteps(t *testing.T) {
	for text, want := range map[string]Step{
		"@review":      {Gate: GateReview},
		"@review:opus": {Gate: GateReview, Model: "opus"},
	} {
		got, err := ParseStep(text)
		if err != nil || got != want || got.String() != text {
			t.Errorf("ParseStep(%q) = %+v, %v", text, got, err)
		}
	}

	var steps []Step
	if err := json.Unmarshal([]byte(`["@review", {"gate": "review", "fix_role": "fixer", "max_rounds": 3}]`), &steps); err != nil {
		t.Fatal(err)
	}
	if want := []Step{{Gate: GateReview}, {Gate: GateReview, FixRole: "fixer", MaxRounds: 3}}; !slices.Equal(steps, want) {
		t.Errorf("steps = %+v", steps)
	}
	if b, _ := json.Marshal(steps); string(b) != `["@review",{"gate":"review","fix_role":"fixer","max_rounds":3}]` {
		t.Errorf("marshal = %s", b)
	}

	for _, tt := range []struct {
		step Step
		want string
	}{
		{Step{Gate: "lint"}, "unknown gate @lint"},
		{Step{Gate: GateReview, Role: "test"}, "a role or a gate"},
		{Step{Gate: GateReview, MaxRounds: -1}, "max_rounds can't be negative"},
		{Step{Role: "test", FixRole: "x"}, "only for gates"},
	} {
		if err := CheckGate(tt.step); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("CheckGate(%+v) = %v, want %q", tt.step, err, tt.want)
		}
	}
}

func TestExpectValidation(t *testing.T) {
	for _, tt := range []struct {
		s    Step
		want string
	}{
		{Step{Role: "test", Expect: "tidy"}, `step test: expect must be "commits", "read-only" or "clean", not "tidy"`},
		{Step{Gate: GateCheck, Expect: ExpectCommits}, "step @check: expect is only for roles"},
	} {
		if err := CheckGate(tt.s); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("CheckGate(%+v) = %v, want %q", tt.s, err, tt.want)
		}
	}
	for _, v := range []string{ExpectCommits, ExpectReadOnly, ExpectClean} {
		if err := CheckGate(Step{Role: "test", Expect: v}); err != nil {
			t.Errorf("expect %s: %v", v, err)
		}
	}

	if b, _ := json.Marshal(Step{Role: "test", Expect: ExpectClean}); string(b) != `{"role":"test","expect":"clean"}` {
		t.Errorf("a step with expect encodes as %s", b)
	}

	home := t.TempDir()
	writeFileT(t, filepath.Join(home, "app", FileName), `{"expect": "sometimes", "pipelines": {}}`)
	if _, _, err := Read(home, "app"); err == nil || !strings.Contains(err.Error(), `expect must be "commits", "read-only" or "clean", not "sometimes"`) {
		t.Errorf("Read = %v", err)
	}
}
