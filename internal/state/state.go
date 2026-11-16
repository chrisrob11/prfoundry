// Package state is a job's state.json: where the job is now, what it has
// spent, and what resume needs to carry it on. It reads and writes the file,
// finds and lists jobs, and sums their spend.
package state

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/chrisrob11/prfoundry/internal/atomicfile"
	"github.com/chrisrob11/prfoundry/internal/pipeline"
)

// FileName is <job>/state.json: where a job is now, rewritten at every change.
const FileName = "state.json"

// Job states. A job that says it's in setup or running but whose process is
// gone is reported as died.
const (
	Setup   = "setup"
	Running = "running"
	Review  = "review" // waiting at a review gate for your review
	Ask     = "ask"    // waiting at an ask gate for your answers
	Paused  = "paused" // stopped by the usage limit; re-runs its step at ResumeAt
	Done    = "done"
	Failed  = "failed"
	Died    = "died"
)

// Job is what's in a job's state.json.
type Job struct {
	Repo       string          `json:"repo"`
	Job        string          `json:"job"`
	Branch     string          `json:"branch"`
	Pipeline   string          `json:"pipeline,omitempty"`
	Roles      []string        `json:"roles,omitempty"`
	Models     []string        `json:"models,omitempty"` // each role's model; "" means Claude's default
	Steps      []pipeline.Step `json:"steps,omitempty"`  // the pipeline as given, with gate settings, for resume
	State      string          `json:"state"`
	Stage      string          `json:"stage"`              // a setup step, or the role running
	Step       int             `json:"step"`               // 1-based position of that role; 0 during setup
	Activity   string          `json:"activity,omitempty"` // Claude's latest tool call
	Exit       *int            `json:"exit,omitempty"`
	Pushed     bool            `json:"pushed,omitempty"`
	PRURL      string          `json:"pr_url,omitempty"`
	ReviewURL  string          `json:"review_url,omitempty"`  // the review page, while waiting at a review or ask gate
	ResumeAt   time.Time       `json:"resume_at,omitzero"`    // when a paused job re-runs its step
	BaseCommit string          `json:"base_commit,omitempty"` // where the branch's changes start, for the review diff
	PR         string          `json:"pr,omitempty"`          // the PR spec, for resume
	Task       string          `json:"task,omitempty"`        // the --prompt, for resume
	Push       bool            `json:"push,omitempty"`        // shipping settings, for resume
	PRCreate   string          `json:"pr_create,omitempty"`
	Cost       float64         `json:"cost"`
	Dir        string          `json:"dir"`
	JobDir     string          `json:"job_dir"`
	PID        int             `json:"pid"`
	Started    time.Time       `json:"started"`
	Updated    time.Time       `json:"updated"`

	// Each role step's HEAD when it last finished, by step number, so a
	// re-run can start from what changed since.
	StepHeads map[int]string `json:"step_heads,omitempty"`

	// Spend is what each role cost, by role and model, summed over every
	// run of it in this job: fix rounds, re-runs and resumes included.
	Spend []Spend `json:"spend,omitempty"`
}

// Spend is one role's spend on one model.
type Spend struct {
	Role      string  `json:"role"`
	Model     string  `json:"model,omitempty"`
	Runs      int     `json:"runs"`
	Turns     int     `json:"turns"`
	Cost      float64 `json:"cost"`
	CacheRead int64   `json:"cache_read_tokens"`
}

// AddSpend adds r, one or more runs of a role on a model, to the job's spend.
func (s *Job) AddSpend(r Spend) {
	s.Spend = addSpend(s.Spend, r)
}

// addSpend adds r to the row in rows for its role and model, adding the row
// if there's none.
func addSpend(rows []Spend, r Spend) []Spend {
	i := slices.IndexFunc(rows, func(x Spend) bool { return x.Role == r.Role && x.Model == r.Model })
	if i < 0 {
		rows = append(rows, Spend{Role: r.Role, Model: r.Model})
		i = len(rows) - 1
	}
	rows[i].Runs += r.Runs
	rows[i].Turns += r.Turns
	rows[i].Cost += r.Cost
	rows[i].CacheRead += r.CacheRead
	return rows
}

// Write rewrites jobDir's state.json with s.
func Write(jobDir string, s Job) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(filepath.Join(jobDir, FileName), append(b, '\n'))
}

// List returns every job under workdir (only repo's, if set), with dead
// processes marked, most recently updated first.
func List(workdir, repo string) ([]Job, error) {
	pattern := filepath.Join(workdir, "*", "*", FileName)
	if repo != "" {
		pattern = filepath.Join(workdir, repo, "*", FileName)
	}
	paths, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	var states []Job
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var s Job
		if json.Unmarshal(b, &s) != nil {
			continue
		}
		if s.Live() && !ProcessAlive(s.PID) {
			s.State = Died
		}
		states = append(states, s)
	}
	slices.SortFunc(states, func(a, b Job) int { return b.Updated.Compare(a.Updated) })
	return states, nil
}

// Live reports whether the job says its process is still working on it.
func (s Job) Live() bool {
	return s.State == Setup || s.State == Running || s.State == Review || s.State == Ask || s.State == Paused
}

// ProcessAlive reports whether a process with this pid exists.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Find returns the state of the job named by target: <repo>/<job> under
// workdir, or a job folder's path.
func Find(workdir, target string) (Job, error) {
	jobDir := target
	if _, err := os.Stat(filepath.Join(jobDir, FileName)); err != nil {
		jobDir = filepath.Join(workdir, target)
	}
	var s Job
	b, err := os.ReadFile(filepath.Join(jobDir, FileName))
	if err != nil {
		return s, fmt.Errorf("no job %q: give <repo>/<job> as prf status shows it, or a job folder", target)
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("%s: %w", filepath.Join(jobDir, FileName), err)
	}
	// A job folder found from the working directory is made absolute: steps
	// run in its repo folder, where a relative path names something else.
	if s.JobDir, err = filepath.Abs(jobDir); err != nil {
		return s, err
	}
	return s, nil
}

// Sum adds up the spend of the jobs active within since of now (every job
// when since is 0), by role and model, most expensive first. total is
// every counted job's cost; unrecorded is the part in jobs with no spend.
func Sum(states []Job, since time.Duration, now time.Time) (rows []Spend, total, unrecorded float64) {
	for _, s := range states {
		if since > 0 && s.Updated.Before(now.Add(-since)) {
			continue
		}
		total += s.Cost
		if len(s.Spend) == 0 {
			unrecorded += s.Cost
			continue
		}
		for _, r := range s.Spend {
			rows = addSpend(rows, r)
		}
	}
	slices.SortStableFunc(rows, func(a, b Spend) int { return cmp.Compare(b.Cost, a.Cost) })
	return rows, total, unrecorded
}
