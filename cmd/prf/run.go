package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/chrisrob11/prfoundry/internal/claude"
	"github.com/chrisrob11/prfoundry/internal/config"
	"github.com/chrisrob11/prfoundry/internal/gitx"
	"github.com/chrisrob11/prfoundry/internal/human"
	"github.com/chrisrob11/prfoundry/internal/jobenv"
	"github.com/chrisrob11/prfoundry/internal/pipeline"
	"github.com/chrisrob11/prfoundry/internal/roles"
	"github.com/chrisrob11/prfoundry/internal/state"
	"github.com/chrisrob11/prfoundry/internal/textfile"
	"github.com/urfave/cli/v3"
)

// event prints a status line to stderr and appends it to the status file, so
// the file's last line is always the job's current state.
func (j *job) event(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	_, _ = fmt.Fprintln(stderr, line)
	if err := appendLine(j.status, line); err != nil {
		logf("writing status: %v", err)
	}
}

// finish records the completion line and exits with code.
func (j *job) finish(last string, code int) error {
	j.event("PRF_DONE branch=%s phase=%s exit=%d dir=%s", j.branch, last, code, j.dir)
	j.setState(func(s *state.Job) {
		s.State, s.Exit, s.Activity = state.Done, &code, ""
		if code != 0 {
			s.State = state.Failed
		}
	})
	return cli.Exit("", code)
}

// heartbeatEvery is how often a "working" line is printed while a phase runs.
var heartbeatEvery = 30 * time.Second

// runPhases runs each phase in j.dir, stopping at the first that fails.
// Progress goes to stderr: a line when each phase starts and ends, and a
// "working" line every heartbeatEvery. Claude's steps go to the log (and,
// with --verbose, to stderr too); its raw stream to <job>/claude.jsonl; its
// final summary of each phase to stdout. A review gate waits for your review
// instead, running its fix role on each round of requested changes; an ask
// gate shows you the question the step before it left, if any; a check gate
// runs a command, having its fix role fix what fails. Every step gets <job>/tmp as PRF_TMP.
func (j *job) runPhases(phases []phase) (err error) {
	logFile, err := openAppend(j.out)
	if err != nil {
		logf("opening output: %v", err)
		return j.finish("setup", exitSetup)
	}
	defer closeInto(&err, logFile)
	rawFile, err := openAppend(filepath.Join(j.jobDir, "claude.jsonl"))
	if err != nil {
		logf("opening output: %v", err)
		return j.finish("setup", exitSetup)
	}
	defer closeInto(&err, rawFile)
	if err := j.useJobTmp(); err != nil {
		logf("making the job's folders: %v", err)
		return j.finish("setup", exitSetup)
	}
	j.log, j.raw, j.summaries, j.feed = logFile, rawFile, io.Discard, logFile
	if j.tee {
		j.summaries = j.stdout
	}
	if j.verbose {
		j.feed = io.MultiWriter(logFile, stderr)
	}

	// Catch Ctrl-C, TERM and a closed terminal (HUP) so Claude (same process
	// group) handles them and we still report PRF_DONE afterwards. A gate
	// waits on them, and a check gate stops its command, which has a group
	// of its own.
	j.sigs = make(chan os.Signal, 1)
	signal.Notify(j.sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(j.sigs)

	j.pipeline = firstNonEmpty(j.pipeline, phases[0].name)
	pipelineName := j.pipeline
	total := j.totalSteps // a resumed job keeps the whole pipeline's numbering
	if total == 0 {
		total = len(phases)
	}
	names, models := make([]string, len(phases)), make([]string, len(phases))
	for i, p := range phases {
		names[i], models[i] = p.name, firstNonEmpty(j.model, p.model, j.defaultModel)
	}
	base := ""
	if j.state.BaseCommit == "" {
		base = j.baseCommit() // before any step commits, for the review gate's diff
	}
	j.setState(func(s *state.Job) {
		s.Pipeline, s.PR, s.Task, s.Push, s.PRCreate = pipelineName, j.pr, j.task, j.push, j.prCreate
		if j.firstStep == 0 {
			s.Roles, s.Models, s.Steps = names, models, j.roles
		} else if len(s.Models) == total {
			copy(s.Models[j.firstStep:], models) // a resume may pick other models
		}
		if base != "" {
			s.BaseCommit = base
		}
	})
	data := roles.Data{PR: j.pr, Branch: j.branch, Task: j.task, Context: j.contextFiles}
	start := time.Now()
	j.cost = j.priorCost
	for i, p := range phases {
		step, model := fmt.Sprintf("%d/%d", j.firstStep+i+1, total), models[i]
		j.setState(func(s *state.Job) { s.Step = j.firstStep + i + 1 })
		var code int
		switch p.gate {
		case pipeline.GateReview:
			code = j.reviewGate(p, step, model, data)
		case pipeline.GateAsk:
			code = j.askGate(p, step, model, data)
		case pipeline.GateCheck:
			code = j.commandGate(p, step, model, data)
		default:
			n := j.firstStep + i + 1
			code = j.runClaude(p, p.name, step, model, j.priorRun(n, data), i == 0)
			j.recordRun(n)
		}

		if code != 0 || i == len(phases)-1 {
			done := j.firstStep + i
			if code == 0 {
				done++
			}
			costNote := ""
			if j.cost > 0 {
				costNote = ", " + human.USD(j.cost)
			}
			logf("pipeline %s: %d/%d steps succeeded in %s%s", pipelineName, done, total, human.Duration(time.Since(start)), costNote)
			if code == 0 {
				code = j.ship()
			}
			return j.finish(p.name, code)
		}
	}
	return nil
}

// runClaude runs phase p's Claude call, and when Claude stops it at the
// usage limit, pauses the job until the limit resets and runs it again, so
// a limit never fails a job. It returns runClaudeOnce's code, or 130 if a
// signal stops a pause.
func (j *job) runClaude(p phase, name, step, model string, data roles.Data, first bool) int {
	for {
		code := j.runClaudeOnce(p, name, step, model, data, first)
		reset, hit := j.lastResult.LimitReset(time.Now())
		if code == 0 || !hit {
			return code
		}
		if !j.pauseUntil(step, name, reset) {
			return 130
		}
		first = false // a streamed --in was read by the first try
	}
}

// runClaudeOnce runs one Claude call for phase p, under name (the role) at
// step, and returns its exit code: Claude's own, 1 if it reported an
// error, or one of prf's for a setup, usage or budget failure, or for a
// role that broke its expectation. Its final message is in j.lastResult.
func (j *job) runClaudeOnce(p phase, name, step, model string, data roles.Data, first bool) int {
	label := name // the phase as progress lines show it
	if model != "" {
		label += " (" + model + ")"
	}
	j.lastResult = claude.Result{}
	prompt, err := p.render(data)
	if err != nil {
		logf("rendering role %s: %v", name, err)
		return exitUsage
	}
	budget := firstPositive(j.stepCost, p.maxCost, j.defaultStep)
	if j.maxCost > 0 {
		left := math.Round((j.maxCost-j.cost)*100) / 100 // to the cent, so float error can't leave a sliver
		if left <= 0 {
			logf("[%s] %s: not started: the job has spent %s of its %s cap", step, label, human.USD(j.cost), human.USD(j.maxCost))
			j.event("PRF_PHASE branch=%s phase=%s step=%s exit=%d stopped=max_cost", j.branch, name, step, exitBudget)
			return exitBudget
		}
		if budget == 0 || left < budget {
			budget = left
		}
	}

	call := claude.Call{Prompt: prompt, Mode: j.mode, Model: model, Budget: budget, Settings: j.settings, Extra: j.claudeArgs}
	if j.pr != "" {
		call.Dirs = append(call.Dirs, filepath.Dir(j.pr))
	}
	if j.tmp != "" {
		call.Dirs = append(call.Dirs, j.tmp) // so the sandbox lets it write there
	}
	call.Dirs = append(call.Dirs, j.writeDirs...)
	if len(j.contextFiles) > 0 {
		call.Dirs = append(call.Dirs, j.contextDir)
	}
	cmd := exec.Command(j.claude, call.Args()...)
	cmd.Dir, cmd.Env, cmd.Stderr = j.dir, j.env, j.feed
	stream, err := cmd.StdoutPipe()
	if err != nil {
		logf("running claude: %v", err)
		return exitSetup
	}
	stdin, err := openInput(j.in, first)
	if err != nil {
		logf("opening --in: %v", err)
		return exitSetup
	}
	cmd.Stdin = stdin

	modelField := ""
	if model != "" {
		modelField = " model=" + model
	}
	pipelineName := j.pipeline
	j.event("PRF_START branch=%s phase=%s step=%s pipeline=%s%s", j.branch, name, step, pipelineName, modelField)
	j.setState(func(s *state.Job) { s.State, s.Stage, s.Activity, s.ReviewURL = state.Running, name, "", "" })
	logf("[%s] %s: started", step, label)
	_, _ = fmt.Fprintf(j.feed, "=== prf phase %s (step %s, pipeline %s%s) ===\n", name, step, pipelineName, strings.ReplaceAll(modelField, "=", " "))
	phaseStart, prog := time.Now(), &claude.Progress{}
	expect, startHead := j.expectOf(p), ""
	if expect != "" {
		startHead = gitx.Output(j.dir, "rev-parse", "HEAD")
	}
	if err := cmd.Start(); err != nil {
		logf("running claude: %v", err)
		return exitSetup
	}
	stopHeartbeat, heartbeatDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		heartbeat(heartbeatEvery, step, label, phaseStart, prog, stopHeartbeat, func(last string) {
			j.setState(func(s *state.Job) { s.Activity = last })
		})
	}()
	res := claude.Follow(stream, j.raw, j.feed, prog, j.dir)
	err = cmd.Wait()
	close(stopHeartbeat)
	<-heartbeatDone // so no "working" line follows the phase's result
	if f, ok := stdin.(*os.File); ok && f != os.Stdin {
		_ = f.Close() // read-only
	}
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			logf("running claude: %v", err)
			return exitSetup
		}
	}

	code := cmd.ProcessState.ExitCode()
	if code < 0 { // killed by a signal
		code = 130
	}
	if res.IsError && code == 0 {
		code = 1 // Claude said the run failed even though it exited cleanly
	}
	overBudget := res.OverBudget()
	if overBudget {
		code = exitBudget
	}
	if res.Text != "" {
		_, _ = fmt.Fprintf(j.log, "--- %s summary ---\n%s\n", name, res.Text)
		_, _ = fmt.Fprintf(j.summaries, "%s\n", res.Text)
	}
	j.lastResult = res
	j.cost += res.Cost
	j.setState(func(s *state.Job) {
		s.Cost = j.cost
		if res.Got {
			s.AddSpend(state.Spend{Role: name, Model: model, Runs: 1, Turns: res.Turns, Cost: res.Cost, CacheRead: res.CacheRead})
		}
	})
	broke := ""
	if code == 0 && expect != "" && !j.asked(p) {
		if broke = j.brokenExpectation(expect, startHead); broke != "" {
			code = exitExpect
		}
	}

	details := ""
	if res.Got {
		details = fmt.Sprintf(" error=%t turns=%d cost=%.2f", res.IsError, res.Turns, res.Cost)
	}
	if overBudget {
		details += " stopped=max_cost"
	}
	if broke != "" {
		details += " expect=" + expect
	}
	j.event("PRF_PHASE branch=%s phase=%s step=%s exit=%d%s", j.branch, name, step, code, details)
	took := human.Duration(time.Since(phaseStart))
	switch {
	case code == 0:
		logf("[%s] %s: done in %s%s", step, label, took, resultNote(res))
	case overBudget:
		logf("[%s] %s: stopped after %s: reached its %s budget (%s spent)", step, label, took, human.USD(budget), human.USD(res.Cost))
	case broke != "":
		logf("[%s] %s: failed after %s: expected %s, but it %s", step, label, took, expect, broke)
	default:
		logf("[%s] %s: failed (exit %d) after %s%s", step, label, code, took, resultNote(res))
	}
	return code
}

// expectOf is what role phase p must leave behind: its own expect, else the
// repo's. Gates and a bare --prompt have none.
func (j *job) expectOf(p phase) string {
	if p.gate != "" || p.tmpl == nil {
		return ""
	}
	return firstNonEmpty(p.expect, j.expect)
}

// asked reports whether role phase p stopped to ask the ask gate after it a
// question. Its expectation waits for the run that doesn't ask.
func (j *job) asked(p phase) bool {
	if p.asks == "" {
		return false
	}
	b, err := os.ReadFile(j.questionPath(p.asks))
	return err == nil && strings.TrimSpace(string(b)) != ""
}

// brokenExpectation says how the checkout breaks expect, given HEAD as it was
// when the step started, or "" if it doesn't.
func (j *job) brokenExpectation(expect, start string) string {
	var broke []string
	head := gitx.Output(j.dir, "rev-parse", "HEAD")
	switch {
	case expect == pipeline.ExpectCommits && head == start:
		broke = append(broke, "made no commits")
	case expect == pipeline.ExpectReadOnly && head != start:
		broke = append(broke, fmt.Sprintf("made commits: HEAD moved from %s to %s", shortSHA(start), shortSHA(head)))
	}
	if status := gitx.Output(j.dir, "status", "--porcelain"); status != "" {
		lines := strings.Split(status, "\n")
		shown := lines[:min(len(lines), 5)]
		for i := range shown {
			shown[i] = strings.TrimSpace(shown[i])
		}
		changes := strings.Join(shown, ", ")
		if more := len(lines) - len(shown); more > 0 {
			changes += fmt.Sprintf(" and %d more", more)
		}
		broke = append(broke, "left uncommitted changes: "+changes)
	}
	return strings.Join(broke, ", and ")
}

// shortSHA is a commit's first 7 characters.
func shortSHA(sha string) string {
	return sha[:min(len(sha), 7)]
}

// priorRun adds to data what step n's last run in this job left: the commit
// it ended at, and its report if it saved one.
func (j *job) priorRun(n int, data roles.Data) roles.Data {
	data.PriorRun = j.state.StepHeads[n]
	if _, err := os.Stat(j.reportPath(n)); err == nil && data.PriorRun != "" {
		data.PriorReport = j.reportPath(n)
	}
	return data
}

// recordRun saves where step n's run ended, and its final message as its
// report, for the next time the step runs.
func (j *job) recordRun(n int) {
	head := gitx.Output(j.dir, "rev-parse", "HEAD")
	j.setState(func(s *state.Job) {
		if s.StepHeads == nil {
			s.StepHeads = map[int]string{}
		}
		s.StepHeads[n] = head
	})
	path := j.reportPath(n)
	if j.lastResult.Text == "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) { // so a later run isn't pointed at an older report
			logf("warning: removing an older report: %v", err)
		}
		return
	}
	if err := textfile.Write(path, j.lastResult.Text+"\n"); err != nil {
		logf("saving step %d's report: %v", n, err)
	}
}

// reportPath is where step n's last report is kept: in the job's tmp
// folder, which Claude can read.
func (j *job) reportPath(n int) string {
	return filepath.Join(j.tmp, "reports", fmt.Sprintf("step-%d.md", n))
}

// useJobTmp makes <job>/tmp and points the steps' PRF_TMP at it, so jobs
// running side by side don't share files, like an ask gate's question. It
// isn't their TMPDIR: Claude Code's sandbox sets its own, and a job's folder
// can be too deep for a unix socket's path.
func (j *job) useJobTmp() error {
	j.tmp = filepath.Join(j.jobDir, "tmp")
	if err := os.MkdirAll(j.tmp, 0o755); err != nil {
		return err
	}
	if j.env == nil {
		j.env = os.Environ()
	}
	j.env = append(j.env, "PRF_TMP="+j.tmp)
	return j.useJobWriteDirs()
}

// useJobWriteDirs makes the folders job_env and job_seed point at, so the
// sandbox can let steps write there. Only a clone in the job folder gets
// them: that's where <job>/mise.toml applies.
func (j *job) useJobWriteDirs() error {
	j.writeDirs = nil
	if !strings.HasPrefix(j.dir, j.jobDir+string(filepath.Separator)) {
		return nil
	}
	repo := j.cfg.Repos[j.name]
	for _, d := range jobenv.WriteDirs(j.jobDir, repo.JobEnv, repo.JobSeed) {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
		j.writeDirs = append(j.writeDirs, d)
	}
	return nil
}

// baseCommit is where the branch's changes start: its merge base with the
// base branch, or HEAD when there's no base branch to compare with.
func (j *job) baseCommit() string {
	refs := []string{"origin/HEAD", "origin/main", "origin/master", "main", "master"}
	if j.base != "" {
		refs = append([]string{"origin/" + j.base, j.base}, refs...)
	}
	for _, ref := range refs {
		if mb := gitx.Output(j.dir, "merge-base", ref, "HEAD"); mb != "" {
			return mb
		}
	}
	return gitx.Output(j.dir, "rev-parse", "HEAD")
}

// stateEvery is how often a running phase's latest activity is saved.
const stateEvery = 2 * time.Second

// heartbeat prints a "working" line every interval, and passes Claude's
// latest activity to save every stateEvery, until stop closes.
func heartbeat(interval time.Duration, step, name string, start time.Time, prog *claude.Progress, stop <-chan struct{}, save func(last string)) {
	tick, saveTick := time.NewTicker(interval), time.NewTicker(min(stateEvery, interval))
	defer tick.Stop()
	defer saveTick.Stop()
	saved := ""
	for {
		select {
		case <-stop:
			return
		case <-saveTick.C:
			if _, last := prog.Snapshot(); last != saved {
				save(last)
				saved = last
			}
		case <-tick.C:
			tools, last := prog.Snapshot()
			msg := fmt.Sprintf("[%s] %s: working (%s, %d tool calls", step, name, human.Duration(time.Since(start)), tools)
			if last != "" {
				msg += "; last: " + last
			}
			logf("%s)", msg)
		}
	}
}

// resultNote describes Claude's result event for a progress line.
func resultNote(res claude.Result) string {
	if !res.Got {
		return ""
	}
	note := fmt.Sprintf(" (%d turns, $%.2f)", res.Turns, res.Cost)
	if res.IsError {
		note = fmt.Sprintf(" (Claude reported an error: %s)", human.OneLine(res.Text, 160))
	}
	return note
}

// openAppend opens path for appending, creating it and its parent dirs.
func openAppend(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

// openInput returns Claude's stdin for one phase. A regular file is reopened
// for every phase; a stream (fifo or '-') can only be read once, so it goes to
// the first phase and later phases get nothing.
func openInput(in string, first bool) (io.Reader, error) {
	switch in {
	case "":
		return nil, nil
	case "-":
		if first {
			return os.Stdin, nil
		}
		return nil, nil
	}
	fi, err := os.Stat(in)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() && !first {
		return nil, nil
	}
	return os.Open(in) // blocks on a fifo until a writer connects
}

// appendLine appends line to path, creating parent directories.
func appendLine(path, line string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(f, line); err != nil {
		return errors.Join(err, f.Close())
	}
	return f.Close()
}

// closeInto closes f, adding a failure to *err only when there is one, so an
// error that was already there keeps its type (a cli.ExitCoder stays one).
func closeInto(err *error, f io.Closer) {
	if cerr := f.Close(); cerr != nil {
		*err = errors.Join(*err, cerr)
	}
}

// statfs reads a filesystem's size and free space; tests replace it.
var statfs = syscall.Statfs

// checkDisk refuses to start when the filesystem holding path is at least
// limit percent full. A limit of 0 checks nothing.
func checkDisk(path string, limit int) error {
	if limit <= 0 {
		return nil
	}
	used, err := diskUsed(path)
	if err != nil {
		return fmt.Errorf("checking free space for %s: %w", path, err)
	}
	if used >= float64(limit) {
		return fmt.Errorf("the disk holding %s is %.0f%% full, at or above max_disk_percent (%d%%) in %s; free some space or raise it", path, used, limit, config.FileName)
	}
	return nil
}

// diskUsed is how full the filesystem holding path is, in percent, counted
// as df does: blocks in use over those in use plus those you may still use.
// A path that doesn't exist yet is on its nearest parent's filesystem.
func diskUsed(path string) (float64, error) {
	for {
		var st syscall.Statfs_t
		err := statfs(path, &st)
		if err == nil {
			used := st.Blocks - st.Bfree
			if used+st.Bavail == 0 {
				return 0, nil
			}
			return 100 * float64(used) / float64(used+st.Bavail), nil
		}
		parent := filepath.Dir(path)
		if !errors.Is(err, fs.ErrNotExist) || parent == path {
			return 0, err
		}
		path = parent
	}
}

// cloneRepo clones repo into dir and switches to branch. If reference is a
// local checkout of the same repo, its objects are copied instead of
// downloaded (--reference --dissociate), so the clone is fast and doesn't
// depend on the checkout afterwards.
func cloneRepo(repo, base, branch, dir, reference string) error {
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("destination already exists: %s", dir)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}

	cloneArgs := []string{"clone", "--quiet"}
	if reference != "" && gitx.OK(reference, "rev-parse", "--git-dir") {
		logf("cloning %s -> %s (objects from %s)", repo, dir, reference)
		cloneArgs = append(cloneArgs, "--reference", reference, "--dissociate")
	} else {
		logf("cloning %s -> %s", repo, dir)
	}
	if base != "" {
		cloneArgs = append(cloneArgs, "--branch", base)
	}
	if err := gitx.Run("", append(cloneArgs, repo, dir)...); err != nil {
		return err
	}
	return switchBranch(dir, branch)
}

// switchBranch switches to branch if it exists locally or on origin, and
// creates it from the current HEAD otherwise.
func switchBranch(dir, branch string) error {
	if gitx.OK(dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch) ||
		gitx.OK(dir, "ls-remote", "--exit-code", "--heads", "origin", branch) {
		logf("switching to existing branch %s", branch)
		return gitx.Run(dir, "switch", "--quiet", branch)
	}
	logf("creating branch %s", branch)
	return gitx.Run(dir, "switch", "--quiet", "-c", branch)
}
