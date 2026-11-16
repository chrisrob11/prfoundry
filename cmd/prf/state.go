package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/chrisrob11/prfoundry/internal/atomicfile"
	"github.com/chrisrob11/prfoundry/internal/config"
	"github.com/chrisrob11/prfoundry/internal/jsonio"
	"github.com/chrisrob11/prfoundry/internal/pipeline"
	"github.com/urfave/cli/v3"
)

// stateFile is <job>/state.json: where a job is now, rewritten at every change.
const stateFile = "state.json"

// Job states. A job that says it's in setup or running but whose process is
// gone is reported as died.
const (
	stateSetup   = "setup"
	stateRunning = "running"
	stateReview  = "review" // waiting at a review gate for your review
	stateAsk     = "ask"    // waiting at an ask gate for your answers
	statePaused  = "paused" // stopped by the usage limit; re-runs its step at ResumeAt
	stateDone    = "done"
	stateFailed  = "failed"
	stateDied    = "died"
)

type jobState struct {
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
	Spend []roleSpend `json:"spend,omitempty"`
}

// roleSpend is one role's spend on one model.
type roleSpend struct {
	Role      string  `json:"role"`
	Model     string  `json:"model,omitempty"`
	Runs      int     `json:"runs"`
	Turns     int     `json:"turns"`
	Cost      float64 `json:"cost"`
	CacheRead int64   `json:"cache_read_tokens"`
}

// addSpend adds one Claude run's result to the role's spend on model.
func (s *jobState) addSpend(role, model string, res phaseResult) {
	i := slices.IndexFunc(s.Spend, func(r roleSpend) bool { return r.Role == role && r.Model == model })
	if i < 0 {
		s.Spend = append(s.Spend, roleSpend{Role: role, Model: model})
		i = len(s.Spend) - 1
	}
	r := &s.Spend[i]
	r.Runs++
	r.Turns += res.turns
	r.Cost += res.cost
	r.CacheRead += res.cacheRead
}

// setState applies change to the job's state and rewrites state.json. It's
// safe to call from the progress goroutine.
func (j *job) setState(change func(s *jobState)) {
	if j.jobDir == "" {
		return
	}
	j.stateMu.Lock()
	defer j.stateMu.Unlock()
	if j.state.Started.IsZero() {
		j.state = jobState{
			Repo: j.name, Job: filepath.Base(j.jobDir), JobDir: j.jobDir,
			PID: os.Getpid(), Started: time.Now(), State: stateSetup,
		}
	}
	change(&j.state)
	j.state.Branch, j.state.Dir, j.state.Updated = j.branch, j.dir, time.Now()
	b, _ := json.MarshalIndent(j.state, "", "  ")
	if err := atomicfile.Write(filepath.Join(j.jobDir, stateFile), append(b, '\n')); err != nil {
		logf("writing %s: %v", stateFile, err)
	}
}

// setupStage records which setup step the job is on.
func (j *job) setupStage(stage string) {
	j.setState(func(s *jobState) { s.State, s.Stage = stateSetup, "setup: "+stage })
}

// readJobStates returns every job under workdir (only repo's, if set),
// with dead processes marked, most recently updated first.
func readJobStates(workdir, repo string) ([]jobState, error) {
	pattern := filepath.Join(workdir, "*", "*", stateFile)
	if repo != "" {
		pattern = filepath.Join(workdir, repo, "*", stateFile)
	}
	paths, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	var states []jobState
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var s jobState
		if json.Unmarshal(b, &s) != nil {
			continue
		}
		if s.live() && !processAlive(s.PID) {
			s.State = stateDied
		}
		states = append(states, s)
	}
	slices.SortFunc(states, func(a, b jobState) int { return b.Updated.Compare(a.Updated) })
	return states, nil
}

// live reports whether the job says its process is still working on it.
func (s jobState) live() bool {
	return s.State == stateSetup || s.State == stateRunning || s.State == stateReview || s.State == stateAsk || s.State == statePaused
}

// processAlive reports whether a process with this pid exists.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func statusCmd() *cli.Command {
	return &cli.Command{
		Name:  "status",
		Usage: "show every job under the workdir: its state, pipeline step, and what Claude is doing",
		Description: "Reads <workdir>/<repo>/<job>/state.json, which each job rewrites as it moves.\n" +
			"States: setup, running, review (waiting for your review at a @review gate), ask (waiting for\n" +
			"your answers at an @ask gate), paused (the usage limit stopped a step; it re-runs after the reset),\n" +
			"done, failed, and died (says it's running, but its process is gone).",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "repo", Usage: "only this repo's jobs"},
			&cli.StringFlag{Name: "workdir", Value: config.ExpandHome("~/prf-work"), Sources: cli.EnvVars("PRF_WORKDIR"), Usage: "where the job folders are (default: conf.json's workdir, else this)"},
			&cli.BoolFlag{Name: "json", Usage: "print JSON"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			states, err := readJobStates(flagOr(c, "workdir", cfg.Workdir), c.String("repo"))
			if err != nil {
				return cli.Exit("prf: "+err.Error(), exitUsage)
			}
			w := c.Root().Writer
			if c.Bool("json") {
				if states == nil {
					states = []jobState{}
				}
				return jsonio.Write(w, states)
			}
			if len(states) == 0 {
				_, _ = fmt.Fprintln(w, "no jobs")
				return nil
			}
			tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "REPO\tJOB\tSTATE\tSTEP\tSTAGE\tTIME\tDETAIL")
			for _, s := range states {
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.Repo, s.Job, s.State, stepOf(s), stageOf(s), timeOf(s), detailOf(s))
			}
			return tw.Flush()
		},
	}
}

// stageOf is the stage, with the running role's model if it has one.
func stageOf(s jobState) string {
	if s.State == stateAsk {
		return "ask (waiting for owner)"
	}
	if s.Step > 0 && s.Step <= len(s.Models) && s.Models[s.Step-1] != "" {
		return fmt.Sprintf("%s (%s)", s.Stage, s.Models[s.Step-1])
	}
	return s.Stage
}

func stepOf(s jobState) string {
	if s.Step == 0 {
		return "-"
	}
	return fmt.Sprintf("%d/%d", s.Step, len(s.Roles))
}

// timeOf is how long a live job has run, or how long ago a finished one ended.
func timeOf(s jobState) string {
	if s.live() {
		return shortDuration(time.Since(s.Started))
	}
	return shortDuration(time.Since(s.Updated)) + " ago"
}

func detailOf(s jobState) string {
	switch s.State {
	case stateSetup, stateRunning:
		return s.Activity
	case stateReview:
		return "waiting for your review: " + s.ReviewURL
	case stateAsk:
		return s.ReviewURL
	case statePaused:
		return pausedDetail(s)
	case stateDied:
		return fmt.Sprintf("process %d is gone", s.PID)
	}
	var parts []string
	if s.PRURL != "" {
		parts = append(parts, s.PRURL)
	} else if s.Pushed {
		parts = append(parts, "pushed")
	}
	if s.Exit != nil {
		parts = append(parts, fmt.Sprintf("exit %d", *s.Exit))
	}
	if s.Cost > 0 {
		parts = append(parts, fmt.Sprintf("$%.2f", s.Cost))
	}
	return strings.Join(parts, ", ")
}
