package job

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/chrisrob11/prfoundry/internal/agent"
	"github.com/chrisrob11/prfoundry/internal/pipeline"
	"github.com/chrisrob11/prfoundry/internal/roles"
	"github.com/chrisrob11/prfoundry/internal/state"
)

func TestOpenInput(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "ctx.txt")
	writeFile(t, file, "context")
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("regular file goes to every phase", func(t *testing.T) {
		for _, first := range []bool{true, false} {
			r, err := openInput(file, first)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(r)
			_ = r.(io.Closer).Close() // read-only
			if string(b) != "context" {
				t.Errorf("first=%v: read %q", first, b)
			}
		}
	})

	tests := []struct {
		name   string
		in     string
		first  bool
		wantIn io.Reader
	}{
		{name: "none", in: "", first: true, wantIn: nil},
		{name: "stdin first phase", in: "-", first: true, wantIn: os.Stdin},
		{name: "stdin later phase", in: "-", first: false, wantIn: nil},
		{name: "fifo later phase", in: fifo, first: false, wantIn: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := openInput(tt.in, tt.first)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.wantIn {
				t.Errorf("got %v, want %v", got, tt.wantIn)
			}
		})
	}

	if _, err := openInput(filepath.Join(dir, "missing"), true); err == nil {
		t.Error("missing file: want error")
	}
}

func TestRecordRun(t *testing.T) {
	clone := filepath.Join(t.TempDir(), "repo")
	runGit(t, "", "clone", "-q", newOrigin(t), clone)
	j := &Job{dir: clone, jobDir: t.TempDir()}
	j.tmp = filepath.Join(j.jobDir, "tmp")
	head, err := gitOut(clone, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	j.lastResult = agent.Result{Text: "did it"}
	j.recordRun(1)
	if got := j.priorRun(1, roles.Data{}); got.PriorRun != head || got.PriorReport != j.reportPath(1) {
		t.Errorf("after a run with a report: %+v", got)
	}
	if got := j.priorRun(2, roles.Data{}); got.PriorRun != "" || got.PriorReport != "" {
		t.Errorf("a step that never ran: %+v", got)
	}

	j.lastResult = agent.Result{}
	j.recordRun(1)
	if got := j.priorRun(1, roles.Data{}); got.PriorRun != head || got.PriorReport != "" {
		t.Errorf("a run with no report should drop the older one: %+v", got)
	}

	logs := &strings.Builder{}
	j.stderr = logs
	j.tmp = filepath.Join(j.jobDir, state.FileName) // a file, so reports/ can't be made
	j.lastResult = agent.Result{Text: "did it"}
	j.recordRun(1)
	if !strings.Contains(logs.String(), "saving step 1's report: ") {
		t.Errorf("log = %q", logs.String())
	}
}

// expectJob is a job of steps (a JSON array) with expect for role steps
// that set none, and roles that commit, do nothing, leave untracked files,
// or both.
func expectJob(t *testing.T, steps, expect string) *testJob {
	tj := newTestJob(t, parseSteps(t, steps)...)
	tj.o.Pipeline, tj.o.Expect = "p", expect
	writeRole(t, tj.roles, "commit", "COMMIT on {{.Branch}}")
	writeRole(t, tj.roles, "plain", "plain on {{.Branch}}")
	writeRole(t, tj.roles, "dirty", "DIRTY on {{.Branch}}")
	writeRole(t, tj.roles, "both", "COMMIT DIRTY on {{.Branch}}")
	return tj
}

func TestExpect(t *testing.T) {
	for _, tt := range []struct {
		name, steps, expect string
		want                int
		wantLog             string
	}{
		{"commits, and it did", `[{"role": "commit", "expect": "commits"}]`, "", 0, ""},
		{"commits, and it made none", `[{"role": "plain", "expect": "commits"}]`, "", ExitExpect,
			"expected commits, but it made no commits"},
		{"commits, but it left changes too", `[{"role": "both", "expect": "commits"}]`, "", ExitExpect,
			"expected commits, but it left uncommitted changes: ?? dirty1.txt, ?? dirty2.txt, ?? dirty3.txt, ?? dirty4.txt, ?? dirty5.txt and 2 more"},
		{"read-only, and it was", `[{"role": "plain", "expect": "read-only"}]`, "", 0, ""},
		{"read-only, but it committed", `[{"role": "commit", "expect": "read-only"}]`, "", ExitExpect,
			"expected read-only, but it made commits: HEAD moved from "},
		{"read-only, but it committed and left changes", `[{"role": "both", "expect": "read-only"}]`, "", ExitExpect,
			", and left uncommitted changes: ?? dirty1.txt"},
		{"clean, and it was", `[{"role": "commit", "expect": "clean"}]`, "", 0, ""},
		{"clean, but it left changes", `[{"role": "dirty", "expect": "clean"}]`, "", ExitExpect,
			"expected clean, but it left uncommitted changes: ?? dirty1.txt"},
		{"the job's expect is the default", `["plain"]`, "commits", ExitExpect, "made no commits"},
		{"a step's own expect wins", `[{"role": "plain", "expect": "clean"}]`, "commits", 0, ""},
		{"no expect, no check", `["dirty"]`, "", 0, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tj := expectJob(t, tt.steps, tt.expect)
			if code := tj.run(t); code != tt.want {
				t.Fatalf("exit %d, want %d\n%s", code, tt.want, tj.logs(t))
			}
			if logs := tj.logs(t); !strings.Contains(logs, tt.wantLog) {
				t.Errorf("log missing %q:\n%s", tt.wantLog, logs)
			}
			status := readFile(t, filepath.Join(tj.jobDir(), "status"))
			if failed := strings.Contains(status, "exit=10 error=false turns=3 cost=0.05 expect="); failed != (tt.want != 0) {
				t.Errorf("status:\n%s", status)
			}
		})
	}
}

// A step that breaks its expectation stops the job there, and resume runs it again.
func TestExpectResume(t *testing.T) {
	tj := expectJob(t, `[{"role": "plain", "expect": "commits"}, "dirty"]`, "")
	if code := tj.run(t); code != ExitExpect {
		t.Fatalf("run: exit %d, want %d", code, ExitExpect)
	}
	if st := readState(t, tj.jobDir()); st.State != state.Failed || st.Step != 1 || st.Exit == nil || *st.Exit != ExitExpect {
		t.Errorf("state = %+v", st)
	}
	if n := len(tj.calls(t)); n != 1 {
		t.Errorf("ran %d steps, want 1", n)
	}

	writeRole(t, tj.roles, "plain", "COMMIT on {{.Branch}}")
	if code := tj.resume(t); code != 0 {
		t.Fatalf("resume: exit %d", code)
	}
	if status := readFile(t, filepath.Join(tj.jobDir(), "status")); !strings.Contains(status, "phase=plain step=1/2 exit=0") {
		t.Errorf("resume should run step 1 again:\n%s", status)
	}
}

// A role that stops to ask isn't held to its expectation; its re-run is.
func TestExpectWithAsk(t *testing.T) {
	tj := askJob(t, 1, `"@ask"`)
	tj.o.Steps = parseSteps(t, `[{"role": "asker", "expect": "commits"}, {"gate": "ask", "question": ".agent/stop.md"}, "test"]`)
	tj.o.Task = ""
	fakeOwner(t, tj, answer)
	if code := tj.run(t); code != ExitExpect {
		t.Fatalf("exit %d, want %d", code, ExitExpect)
	}
	status := readFile(t, filepath.Join(tj.jobDir(), "status"))
	for _, want := range []string{"phase=asker step=1/3 exit=0", "phase=asker step=2/3 exit=10", "PRF_DONE branch=b phase=@ask exit=10"} {
		if !strings.Contains(status, want) {
			t.Errorf("status missing %q:\n%s", want, status)
		}
	}
}

// A step stopped by the usage limit pauses the job, then runs again and
// the job finishes: the limit never fails it.
func TestALimitPausesTheJobAndRerunsTheStep(t *testing.T) {
	tj := newTestJob(t, roleSteps("implement", "test", "review")...)
	tj.o.Task = "x"
	writeRole(t, tj.roles, "test", "LIMIT")
	limits := filepath.Join(tj.root, "limits")
	writeFile(t, limits, "1\n")
	t.Setenv("FAKE_LIMITS", limits)
	var asked time.Time
	tj.o.Runtime.PauseFor = func(reset time.Time) time.Duration { asked = reset; return 300 * time.Millisecond }

	jobDir := tj.jobDir()
	seen := make(chan state.Job, 1)
	go func() {
		for range 100 {
			b, err := os.ReadFile(filepath.Join(jobDir, state.FileName))
			if err == nil && strings.Contains(string(b), `"state": "paused"`) {
				seen <- readState(t, jobDir)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		close(seen)
	}()

	if code := tj.run(t); code != 0 {
		t.Fatalf("exit %d, want 0: the step runs again after the pause", code)
	}
	if n := len(tj.calls(t)); n != 4 {
		t.Errorf("got %d claude calls, want 4 (implement, test twice, review)", n)
	}
	if asked.UTC().Hour() != 15 {
		t.Errorf("paused for a reset at %v, want 3pm UTC from the message", asked)
	}
	st, ok := <-seen
	if !ok {
		t.Fatal("state.json never said paused")
	}
	if st.State != state.Paused || st.Step != 2 || st.ResumeAt.IsZero() || !st.Live() {
		t.Errorf("paused state = %+v", st)
	}
	if after := readState(t, jobDir); after.State != state.Done || !after.ResumeAt.IsZero() {
		t.Errorf("after: state %s, resume_at %v", after.State, after.ResumeAt)
	}
	if status := readFile(t, filepath.Join(jobDir, "status")); !strings.Contains(status, "PRF_PAUSE branch=b phase=test step=2/3 until=") {
		t.Errorf("status has no pause line:\n%s", status)
	}
}

// A failure that isn't the usage limit still fails the job at once.
func TestAnotherErrorStillFailsAtOnce(t *testing.T) {
	tj := newTestJob(t, roleSteps("implement", "test", "review")...)
	tj.o.Task = "x"
	writeRole(t, tj.roles, "test", "FAIL")
	tj.o.Runtime.PauseFor = func(time.Time) time.Duration { t.Error("paused for a non-limit failure"); return 0 }
	if code := tj.run(t); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
}

// A step that runs again in the same job is told where its last run ended,
// and where its report is. A step running for the first time isn't.
func TestPriorRun(t *testing.T) {
	tj := newTestJob(t, roleSteps("implement", "test", "review")...)
	tj.o.Task = "x"
	writeRole(t, tj.roles, "test", "COMMIT FAIL")
	if code := tj.run(t); code != 1 {
		t.Fatalf("run: exit %d, want 1", code)
	}
	for _, c := range tj.calls(t) {
		if strings.Contains(c[1], "This step ran before") {
			t.Errorf("a first run got a re-run note: %s", c[1])
		}
	}
	jobDir := tj.jobDir()
	st := readState(t, jobDir)
	head, err := gitOut(filepath.Join(jobDir, "repo"), "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.StepHeads) != 2 || st.StepHeads[2] != head || st.StepHeads[1] == "" {
		t.Errorf("step_heads = %v, want step 2 at %s", st.StepHeads, head)
	}
	report := filepath.Join(jobDir, "tmp", "reports", "step-2.md")
	if got := readFile(t, report); got != "it broke\n" {
		t.Errorf("step 2's report = %q", got)
	}

	writeRole(t, tj.roles, "test", "test on {{.Branch}} after {{.PriorRun}}")
	removeFile(t, tj.log)
	if code := tj.resume(t); code != 0 {
		t.Fatalf("resume: exit %d", code)
	}
	calls := tj.calls(t)
	if len(calls) != 2 {
		t.Fatalf("resume ran %d steps, want 2", len(calls))
	}
	note := "## This step ran before\n\nThis step ran before, ending at " + head + ": read `git diff " + head + "..HEAD` and its earlier report (" + report + ") first, and read unchanged code only where you need it."
	if !strings.Contains(calls[0][1], "after "+head) || !strings.Contains(calls[0][1], note) {
		t.Errorf("the re-run's prompt should have the sha and the note:\n%s", calls[0][1])
	}
	if strings.Contains(calls[1][1], "This step ran before") {
		t.Errorf("step 3 never ran before, but got the note:\n%s", calls[1][1])
	}
	if st := readState(t, jobDir); len(st.StepHeads) != 3 {
		t.Errorf("step_heads after resume = %v", st.StepHeads)
	}
}

func TestExpectOf(t *testing.T) {
	j := Job{expect: pipeline.ExpectCommits}
	role, err := parseRole("r", "r")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		p    phase
		want string
	}{
		{role, pipeline.ExpectCommits},
		{phase{name: "r", tmpl: role.tmpl, expect: pipeline.ExpectClean}, pipeline.ExpectClean},
		{phase{name: "@check", tmpl: role.tmpl, gate: pipeline.GateCheck}, ""},
		{phase{name: "prompt"}, ""},
	} {
		if got := j.expectOf(tt.p); got != tt.want {
			t.Errorf("expectOf(%s) = %q, want %q", tt.p.name, got, tt.want)
		}
	}
}
