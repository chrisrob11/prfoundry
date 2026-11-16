package dashboard

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/chrisrob11/prfoundry/internal/human"
	"github.com/chrisrob11/prfoundry/internal/pipeline"
	"github.com/chrisrob11/prfoundry/internal/review"
	"github.com/chrisrob11/prfoundry/internal/state"
)

// Row is one job on the overview.
type Row struct {
	Repo      string    `json:"repo"`
	Job       string    `json:"job"`
	Branch    string    `json:"branch"`
	State     string    `json:"state"`
	Waiting   bool      `json:"waiting"` // at a review or ask gate, waiting for you
	Step      int       `json:"step"`    // 1-based; 0 during setup
	Steps     int       `json:"steps"`
	Stage     string    `json:"stage"`
	Activity  string    `json:"activity,omitempty"`
	Agent     string    `json:"agent"`
	Model     string    `json:"model,omitempty"` // the current step's; "" for the agent's default
	Started   time.Time `json:"started"`
	Updated   time.Time `json:"updated"`
	Elapsed   string    `json:"elapsed"` // how long a live job has run, or a finished one ran
	Cost      float64   `json:"cost"`
	PID       int       `json:"pid"`
	Exit      *int      `json:"exit,omitempty"`
	ResumeAt  time.Time `json:"resume_at,omitzero"`
	PRURL     string    `json:"pr_url,omitempty"`
	ReviewURL string    `json:"review_url,omitempty"`
}

// Overview is GET api/jobs: the jobs that pass the filters, waiting on you
// first, then live ones, then the rest, each group most recently updated first.
type Overview struct {
	Now   time.Time `json:"now"`
	Repos []string  `json:"repos"` // every repo with jobs, filtered or not
	Jobs  []Row     `json:"jobs"`
}

// Detail is GET api/jobs/{repo}/{job}: one job's row, its pipeline, its
// spend and its gates' rounds.
type Detail struct {
	Row
	Pipeline   string         `json:"pipeline,omitempty"`
	Dir        string         `json:"dir"`
	BaseCommit string         `json:"base_commit,omitempty"`
	PR         string         `json:"pr,omitempty"`
	Task       string         `json:"task,omitempty"`
	Pipe       []StepView     `json:"pipeline_steps"`
	Spend      []state.Spend  `json:"spend"`
	Checks     []CheckLog     `json:"checks"`
	Reviews    []review.Round `json:"reviews"`
	Asks       []AskRounds    `json:"asks"`
}

// StepView is one step of a job's pipeline.
type StepView struct {
	N       int    `json:"n"`
	Name    string `json:"name"`           // the role, or @gate
	Gate    string `json:"gate,omitempty"` // review, ask or check
	Model   string `json:"model,omitempty"`
	Run     string `json:"run,omitempty"` // a check gate's command
	FixRole string `json:"fix_role,omitempty"`
	Status  string `json:"status"` // done, current, failed or pending
}

// CheckLog is one round of a @check gate: <job>/checks/step-N/round-K.log.
type CheckLog struct {
	Step  int   `json:"step"`
	Round int   `json:"round"`
	Size  int64 `json:"size"`
}

// AskRounds are an @ask gate's rounds: <job>/asks/step-N/round-K.json.
type AskRounds struct {
	Step   int            `json:"step"`
	Rounds []review.Round `json:"rounds"`
}

// LogTail is GET api/jobs/{repo}/{job}/log: the end of claude.log.
type LogTail struct {
	Lines     []string `json:"lines"`
	Truncated bool     `json:"truncated"` // earlier lines are left out
}

// Diff is GET api/jobs/{repo}/{job}/diff: git diff <base_commit> in the
// job's checkout, committed and uncommitted changes to tracked files.
type Diff struct {
	Base      string `json:"base,omitempty"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`      // Text is the diff's first MaxDiff bytes
	Note      string `json:"note,omitempty"` // why there's no diff
}

// rank orders the overview: waiting on you, then live, then finished.
func rank(st string) int {
	switch st {
	case state.Review, state.Ask:
		return 0
	case state.Setup, state.Running, state.Paused:
		return 1
	}
	return 2
}

func (s *Server) row(j state.Job) Row {
	r := Row{
		Repo: j.Repo, Job: j.Job, Branch: j.Branch, State: j.State, Waiting: rank(j.State) == 0,
		Step: j.Step, Steps: len(j.Roles), Stage: j.Stage, Activity: j.Activity, Agent: cmp.Or(j.Agent, "claude"),
		Started: j.Started, Updated: j.Updated, Cost: j.Cost, PID: j.PID, Exit: j.Exit,
		ResumeAt: j.ResumeAt, PRURL: j.PRURL, ReviewURL: j.ReviewURL,
	}
	if j.Step > 0 && j.Step <= len(j.Models) {
		r.Model = j.Models[j.Step-1]
	}
	if !j.Started.IsZero() {
		end := j.Updated
		if j.Live() {
			end = s.o.Now()
		}
		r.Elapsed = human.Duration(max(end.Sub(j.Started), 0))
	}
	return r
}

func (s *Server) serveJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := state.List(s.o.Workdir, s.o.Repo)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	repo, st := r.URL.Query().Get("repo"), r.URL.Query().Get("state")
	o := Overview{Now: s.o.Now(), Repos: []string{}, Jobs: []Row{}}
	for _, j := range jobs {
		if !slices.Contains(o.Repos, j.Repo) {
			o.Repos = append(o.Repos, j.Repo)
		}
		if (repo == "" || j.Repo == repo) && (st == "" || j.State == st) {
			o.Jobs = append(o.Jobs, s.row(j))
		}
	}
	slices.Sort(o.Repos)
	// List is most recently updated first; keep that within each rank.
	slices.SortStableFunc(o.Jobs, func(a, b Row) int { return rank(a.State) - rank(b.State) })
	writeJSON(w, http.StatusOK, o)
}

// job reads the job the request names, writing a 404 if there's none.
func (s *Server) job(w http.ResponseWriter, r *http.Request) (state.Job, string, bool) {
	repo, name := r.PathValue("repo"), r.PathValue("job")
	if !plainName(repo) || !plainName(name) || (s.o.Repo != "" && repo != s.o.Repo) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such job"})
		return state.Job{}, "", false
	}
	dir := filepath.Join(s.o.Workdir, repo, name)
	j, err := state.Read(dir)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such job"})
		return state.Job{}, "", false
	}
	return j, dir, true
}

// plainName reports whether a path element names a folder in its parent.
func plainName(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, `/\`)
}

func (s *Server) serveJob(w http.ResponseWriter, r *http.Request) {
	j, dir, ok := s.job(w, r)
	if !ok {
		return
	}
	d := Detail{
		Row: s.row(j), Pipeline: j.Pipeline, Dir: j.Dir, BaseCommit: j.BaseCommit, PR: j.PR, Task: j.Task,
		Pipe: steps(j), Spend: j.Spend, Checks: checkLogs(dir), Asks: []AskRounds{},
	}
	var err error
	if d.Reviews, err = review.ReadRounds(filepath.Join(dir, "reviews")); err != nil {
		s.o.Logf("%v", err)
	}
	if d.Spend == nil {
		d.Spend = []state.Spend{}
	}
	if d.Reviews == nil {
		d.Reviews = []review.Round{}
	}
	for _, step := range numbered(filepath.Join(dir, "asks", "step-*"), "step-%d") {
		rounds, err := review.ReadRounds(step.path)
		if err != nil {
			s.o.Logf("%v", err)
		}
		if len(rounds) > 0 {
			d.Asks = append(d.Asks, AskRounds{Step: step.n, Rounds: rounds})
		}
	}
	writeJSON(w, http.StatusOK, d)
}

// steps is j's pipeline, each step marked done, current, failed or pending.
func steps(j state.Job) []StepView {
	views := []StepView{}
	for i, name := range j.Roles {
		st, _ := pipeline.ParseStep(name)
		if len(j.Steps) == len(j.Roles) {
			st = j.Steps[i]
		}
		v := StepView{N: i + 1, Name: name, Gate: st.Gate, Run: st.Run, FixRole: st.FixRole, Status: "pending"}
		if i < len(j.Models) {
			v.Model = j.Models[i]
		}
		switch {
		case v.N < j.Step || (v.N == j.Step && j.State == state.Done):
			v.Status = "done"
		case v.N == j.Step && (j.State == state.Failed || j.State == state.Died):
			v.Status = "failed"
		case v.N == j.Step:
			v.Status = "current"
		}
		views = append(views, v)
	}
	return views
}

// file is a folder or file whose name holds a number, like step-2.
type file struct {
	path string
	n    int
}

// numbered returns the paths matching glob whose base name scans with
// format, in order of their number.
func numbered(glob, format string) []file {
	paths, _ := filepath.Glob(glob) // the pattern is well formed
	var files []file
	for _, p := range paths {
		var n int
		if _, err := fmt.Sscanf(filepath.Base(p), format, &n); err == nil {
			files = append(files, file{p, n})
		}
	}
	slices.SortFunc(files, func(a, b file) int { return a.n - b.n })
	return files
}

// checkLogs lists the job's @check round logs, by step then round.
func checkLogs(dir string) []CheckLog {
	logs := []CheckLog{}
	for _, step := range numbered(filepath.Join(dir, "checks", "step-*"), "step-%d") {
		for _, round := range numbered(filepath.Join(step.path, "round-*.log"), "round-%d.log") {
			if fi, err := os.Stat(round.path); err == nil {
				logs = append(logs, CheckLog{Step: step.n, Round: round.n, Size: fi.Size()})
			}
		}
	}
	return logs
}

// tailBytes is how much of the end of a log the dashboard reads.
const tailBytes = 256 << 10

// tail returns the last bytes of the file at path, at most tailBytes, and
// whether any came before them.
func tail(path string) ([]byte, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	off := max(fi.Size()-tailBytes, 0)
	b, err := io.ReadAll(io.NewSectionReader(f, off, fi.Size()-off))
	return b, off > 0, err
}

func (s *Server) serveLog(w http.ResponseWriter, r *http.Request) {
	_, dir, ok := s.job(w, r)
	if !ok {
		return
	}
	t := LogTail{Lines: []string{}}
	b, cut, err := tail(filepath.Join(dir, "claude.log"))
	if err != nil {
		writeJSON(w, http.StatusOK, t)
		return
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if cut {
		lines = lines[1:] // starts mid-line
	}
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}
	if len(lines) > s.o.MaxLog {
		lines, cut = lines[len(lines)-s.o.MaxLog:], true
	}
	t.Lines, t.Truncated = append(t.Lines, lines...), cut
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) serveDiff(w http.ResponseWriter, r *http.Request) {
	j, _, ok := s.job(w, r)
	if !ok {
		return
	}
	d := Diff{Base: j.BaseCommit}
	switch fi, err := os.Stat(j.Dir); {
	case j.BaseCommit == "":
		d.Note = "no base commit recorded yet: the job is still setting up"
	case err != nil || !fi.IsDir():
		d.Note = "the job's checkout is gone"
	default:
		d.Text, d.Truncated, d.Note = s.diff(r.Context(), j.Dir, j.BaseCommit)
	}
	writeJSON(w, http.StatusOK, d)
}

// diff runs git diff base in dir, reading at most MaxDiff bytes of it.
// note is git's error, when it fails.
func (s *Server) diff(ctx context.Context, dir, base string) (text string, truncated bool, note string) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "diff", "--no-color", "--no-ext-diff", base, "--")
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		return "", false, "running git diff: " + err.Error()
	}
	b, _ := io.ReadAll(io.LimitReader(out, int64(s.o.MaxDiff)+1))
	if len(b) > s.o.MaxDiff {
		cancel() // the rest isn't shown
		b, truncated = b[:s.o.MaxDiff], true
		if i := bytes.LastIndexByte(b, '\n'); i >= 0 {
			b = b[:i+1]
		}
	}
	if err := cmd.Wait(); err != nil && !truncated {
		return "", false, "git diff " + base + ": " + cmp.Or(strings.TrimSpace(stderr.String()), err.Error())
	}
	return string(b), truncated, ""
}

func (s *Server) serveCheck(w http.ResponseWriter, r *http.Request) {
	_, dir, ok := s.job(w, r)
	if !ok {
		return
	}
	step, err1 := strconv.Atoi(r.PathValue("step"))
	round, err2 := strconv.Atoi(r.PathValue("round"))
	if err1 != nil || err2 != nil || step < 1 || round < 1 {
		http.NotFound(w, r)
		return
	}
	b, cut, err := tail(filepath.Join(dir, "checks", fmt.Sprintf("step-%d", step), fmt.Sprintf("round-%d.log", round)))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	if cut {
		_, _ = fmt.Fprintf(w, "[truncated: only the last %d KiB of the log is shown]\n", tailBytes>>10)
	}
	_, _ = w.Write(b)
}
