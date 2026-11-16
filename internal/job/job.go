// Package job runs one prf job: Claude Code headless through a pipeline of
// role steps and gates, in a fresh clone of a repo (Run), a checkout that
// already exists (Apply), or the checkout of an earlier job that stopped
// (Resume). It reports as it goes on stderr, in <job>/status and in
// <job>/state.json, and once every step passes it ships the branch.
package job

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/chrisrob11/prfoundry/internal/agent"
	"github.com/chrisrob11/prfoundry/internal/jobenv"
	"github.com/chrisrob11/prfoundry/internal/pipeline"
	"github.com/chrisrob11/prfoundry/internal/roles"
	"github.com/chrisrob11/prfoundry/internal/state"
)

// prf's exit codes, besides a failed step's own.
const (
	ExitUsage  = 2
	ExitSetup  = 3
	ExitShip   = 4  // every step passed, but pushing or opening the PR failed
	ExitBudget = 5  // a cost cap stopped the job
	ExitReview = 6  // a review gate ran out of rounds with changes still requested
	ExitAsk    = 7  // an ask gate ran out of rounds with the step still asking
	ExitOwner  = 8  // you stopped the job at an ask gate
	ExitCheck  = 9  // a check gate's command still failed after max_rounds fixes
	ExitExpect = 10 // a role step didn't leave the checkout as its expect says
)

// exitError ends prf with code, printing msg unless it's "". The CLI
// passes it on as it is: it has ExitCode, as urfave/cli's ExitCoder does.
type exitError struct {
	msg  string
	code int
}

func (e exitError) Error() string { return e.msg }

func (e exitError) ExitCode() int { return e.code }

// Options are a job's settings, once the CLI has merged its flags,
// conf.json and the repo's pipeline.json.
type Options struct {
	Home     string          // the prf home
	Repo     string          // the repo's short name
	Steps    []pipeline.Step // what to run; none means Task alone is the prompt
	Pipeline string          // the steps' name in progress and status lines
	PR       string          // the PR spec's absolute path, or ""
	Task     string          // --prompt

	Model           string  // for every step, over its own
	DefaultModel    string  // for steps that name none
	MaxCost         float64 // the whole job's budget in USD; 0 for none
	StepCost        float64 // every step's budget, over its own
	DefaultStepCost float64 // for steps that set none
	Expect          string  // for role steps that set none
	Check           string  // the repo's check command, for @check gates without their own

	Push        bool   // push the branch once every step passes
	PRCreate    string // then open a PR: config.PRDraft, config.PRReady, or ""
	Base        string // the PR's base branch; "" lets gh pick
	PRTitleFile string
	PRBodyFile  string

	// JobEnv and JobSeed are conf.json's job_env and job_seed for Repo: in a
	// clone, the folders under the job folder that the steps may write.
	JobEnv  map[string]string
	JobSeed []jobenv.Seed

	Workdir    string      // job folders go in <Workdir>/<Repo>/<job>/
	Out        string      // Claude's step-by-step log; "" for <job>/claude.log, with each step's summary on Stdout
	Status     string      // the status file; "" for <job>/status
	In         string      // extra context on Claude's stdin: a file, or a fifo or "-"
	Verbose    bool        // also print each of Claude's steps on Stderr
	Mode       string      // Claude's permission mode
	Agent      agent.Agent // the coding agent each step runs
	AgentArgs  []string    // conf.json's arguments for the agent, and the command line's
	Sandbox    bool        // run Claude in Claude Code's sandbox
	ReviewOpen bool        // open a gate's page in your browser

	Stdout, Stderr io.Writer
	Runtime        Runtime
}

// Runtime is how a job reaches the world beyond its checkout: your
// browser, your desktop, the clock and the disk. A zero field means the
// default; tests set them to watch a job, or speed it up.
type Runtime struct {
	Heartbeat   time.Duration                                 // how often a running step prints a "working" line; default 30s
	PauseFor    func(reset time.Time) time.Duration           // how long to wait out a usage limit that resets at reset
	OpenBrowser func(url string) error                        // opens a gate's page
	Notify      func(title, msg string)                       // tells you a gate is waiting
	Statfs      func(path string, st *syscall.Statfs_t) error // reads the disk's size and free space, for max_disk_percent
}

// withDefaults fills in r's zero fields.
func (r Runtime) withDefaults() Runtime {
	if r.Heartbeat == 0 {
		r.Heartbeat = 30 * time.Second
	}
	if r.PauseFor == nil {
		r.PauseFor = func(reset time.Time) time.Duration { return time.Until(reset) + limitMargin }
	}
	if r.OpenBrowser == nil {
		r.OpenBrowser = openBrowser
	}
	if r.Notify == nil {
		r.Notify = notify
	}
	if r.Statfs == nil {
		r.Statfs = syscall.Statfs
	}
	return r
}

// commandGrace is how long a command prf stops, or the processes a
// finished command left running, get to exit before they're killed.
const commandGrace = 5 * time.Second

// Job is one prf job: its settings, where it runs, and how far it has got.
type Job struct {
	home         string // the prf home dir
	name         string // short repo name
	workdir      string
	jobDir       string // <workdir>/<name>/<pr or branch>/: holds status and logs
	dir          string // where Claude runs
	branch       string
	pr           string
	task         string
	roles        []pipeline.Step
	phases       []phase
	pipeline     string  // name shown in progress and status lines
	model        string  // --model: overrides every step's model
	defaultModel string  // for steps that don't name a model
	maxCost      float64 // the whole job's budget in USD; 0 means none
	stepCost     float64 // --max-step-cost: overrides every step's budget
	defaultStep  float64 // budget for steps that don't set max_cost
	expect       string  // for role steps that don't set expect
	push         bool    // push the branch once every step passes
	prCreate     string  // then open a PR: "draft", "ready", or ""
	base         string  // the PR's base branch; "" lets gh pick
	prTitleFile  string
	prBodyFile   string
	jobEnv       map[string]string
	jobSeed      []jobenv.Seed
	firstStep    int // steps already done before this run (resume)
	totalSteps   int // steps in the whole pipeline (resume); 0 means len(phases)
	priorCost    float64
	verbose      bool // also print each of Claude's steps to stderr
	outFlag      string
	statusFlag   string
	out          string
	tee          bool      // also print Claude's summary of each phase to stdout
	stdout       io.Writer // the command's stdout
	stderr       io.Writer // progress, status lines and warnings
	status       string
	sandbox      bool
	settings     string   // Claude settings file; empty when the sandbox is off
	env          []string // environment for mise and Claude
	tmp          string   // <job>/tmp: the steps' PRF_TMP and TMPDIR
	writeDirs    []string // folders under the job folder that job_env and job_seed use
	checkCmd     string   // the repo's check command, for @check gates without their own
	contextDir   string   // <job>/context: codebase map and setup output
	contextFiles []string // files in contextDir, listed in every prompt
	stateMu      sync.Mutex
	state        state.Job // what's in <job>/state.json
	in           string
	mode         string
	agent        agent.Agent
	agentArgs    []string
	reviewOpen   bool // open the review page in your browser when a review gate is reached
	rt           Runtime
	grace        time.Duration // commandGrace, which tests shorten

	// Set by runPhases.
	log, raw   io.Writer // Claude's step-by-step log, and its raw stream
	feed       io.Writer // the log, plus stderr with --verbose
	summaries  io.Writer // where each phase's final message goes
	sigs       chan os.Signal
	cost       float64      // spent so far, including before a resume
	lastResult agent.Result // the last agent run's result
}

// New loads every role the job's steps name, so a typo fails before any
// git work happens. Its errors are usage errors.
func New(o Options) (*Job, error) {
	phases, err := loadPhases(o.Steps, roles.Dirs(o.Home, o.Repo), o.PR != "" || o.Task != "", o.Task != "")
	if err != nil {
		return nil, err
	}
	if err := pipeline.CheckCommands(o.Steps, o.Check); err != nil {
		return nil, err
	}
	j := newJob(o)
	j.phases = phases
	return j, nil
}

// newJob is a job with o's settings, before it has a folder or phases.
func newJob(o Options) *Job {
	return &Job{
		home: o.Home, name: o.Repo, roles: o.Steps, pipeline: o.Pipeline, pr: o.PR, task: o.Task,
		model: o.Model, defaultModel: o.DefaultModel, maxCost: o.MaxCost, stepCost: o.StepCost, defaultStep: o.DefaultStepCost,
		expect: o.Expect, checkCmd: o.Check,
		push: o.Push, prCreate: o.PRCreate, base: o.Base, prTitleFile: o.PRTitleFile, prBodyFile: o.PRBodyFile,
		jobEnv: o.JobEnv, jobSeed: o.JobSeed,
		workdir: o.Workdir, outFlag: o.Out, statusFlag: o.Status, in: o.In, verbose: o.Verbose,
		mode: o.Mode, agent: o.Agent, agentArgs: o.AgentArgs, sandbox: o.Sandbox, reviewOpen: o.ReviewOpen,
		stdout: o.Stdout, stderr: o.Stderr, rt: o.Runtime.withDefaults(), grace: commandGrace,
	}
}

func (j *Job) logf(format string, args ...any) {
	_, _ = fmt.Fprintf(j.stderr, "prf: "+format+"\n", args...)
}

// event prints a status line to stderr and appends it to the status file, so
// the file's last line is always the job's current state.
func (j *Job) event(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	_, _ = fmt.Fprintln(j.stderr, line)
	if err := appendLine(j.status, line); err != nil {
		j.logf("writing status: %v", err)
	}
}

// finish records the completion line and exits with code.
func (j *Job) finish(last string, code int) error {
	j.event("PRF_DONE branch=%s phase=%s exit=%d dir=%s", j.branch, last, code, j.dir)
	j.setState(func(s *state.Job) {
		s.State, s.Exit, s.Activity = state.Done, &code, ""
		if code != 0 {
			s.State = state.Failed
		}
	})
	return exitError{code: code}
}

// setState applies change to the job's state and rewrites state.json. It's
// safe to call from the progress goroutine.
func (j *Job) setState(change func(s *state.Job)) {
	if j.jobDir == "" {
		return
	}
	j.stateMu.Lock()
	defer j.stateMu.Unlock()
	if j.state.Started.IsZero() {
		j.state = state.Job{
			Repo: j.name, Job: filepath.Base(j.jobDir), JobDir: j.jobDir,
			PID: os.Getpid(), Started: time.Now(), State: state.Setup,
		}
	}
	change(&j.state)
	j.state.Branch, j.state.Dir, j.state.Updated = j.branch, j.dir, time.Now()
	if err := state.Write(j.jobDir, j.state); err != nil {
		j.logf("writing %s: %v", state.FileName, err)
	}
}

// setupStage records which setup step the job is on.
func (j *Job) setupStage(stage string) {
	j.setState(func(s *state.Job) { s.State, s.Stage = state.Setup, "setup: "+stage })
}

// activity is Claude's latest step, for the review page while it fixes.
func (j *Job) activity() string {
	j.stateMu.Lock()
	defer j.stateMu.Unlock()
	return j.state.Activity
}
