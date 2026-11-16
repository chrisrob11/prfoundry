package job

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

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
