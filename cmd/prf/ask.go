package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/chrisrob11/prfoundry/internal/gitx"
	"github.com/chrisrob11/prfoundry/internal/pipeline"
	"github.com/chrisrob11/prfoundry/internal/roles"
	"github.com/chrisrob11/prfoundry/internal/textfile"
)

// An ask gate (@ask in a pipeline, right after a role) lets that role stop
// and ask you something its brief didn't settle. The role writes its question
// to a file ($PRF_TMP/stop.md, in the job's own tmp folder, unless the gate
// says otherwise) and finishes.
// If the file is there, the gate serves it on the review page, above the
// branch's diff, and waits for your answers. Its fix role records them and
// removes the file; then the gate re-runs the role, with every answer so far
// in its prompt. If the role asks again, so does the gate, up to max_rounds
// questions. With no file, the gate passes at once. Each question and its
// answers are saved in <job>/asks/step-N/round-M.json, and the answers as the
// fix role saw them in round-M.md.

// acceptText is what "Accept recommendations" answers.
const acceptText = "The owner accepts the option marked Recommended for every question."

// askGate runs the ask gate p at step and returns its exit code: 0 once the
// step before it finishes without a question, the fix role's or the step's
// code if either fails, exitAsk when the step asks more than max_rounds
// times, exitOwner if you stop the job, or 130 if interrupted while waiting.
// A resumed gate picks up where its saved rounds left off.
func (j *job) askGate(p phase, step, model string, data roles.Data) int {
	n, _, _ := strings.Cut(step, "/")
	dir := filepath.Join(j.jobDir, "asks", "step-"+n)
	rounds, err := readRounds(dir)
	if err != nil {
		logf("%v", err)
		return exitSetup
	}
	path := j.questionPath(p.question)
	askerModel := firstNonEmpty(j.model, p.asker.model, j.defaultModel)

	j.event("PRF_START branch=%s phase=%s step=%s pipeline=%s", j.branch, p.name, step, j.pipeline)
	var srv *reviewServer // started at the first question
	defer func() {
		if srv != nil {
			srv.close()
		}
	}()
	question := ""
	page := func(state string) {
		if srv != nil {
			srv.set(state, j.askView(p, path, question, rounds, len(rounds)))
		}
	}
	asked := 0
	end := func(code int, note string) int {
		j.event("PRF_PHASE branch=%s phase=%s step=%s exit=%d rounds=%d%s", j.branch, p.name, step, code, len(rounds), note)
		return code
	}
	for {
		if n := len(rounds); n > 0 && rounds[n-1].Verdict != verdictStop && !rounds[n-1].Rerun {
			last := &rounds[n-1]
			if !last.Fixed { // record your answers
				page(pageFixing)
				fix := data
				fix.Question, fix.Review = last.Question, renderAnswers(*last, j.displayPath(path))
				if err := textfile.Write(filepath.Join(dir, fmt.Sprintf("round-%d.md", last.Round)), fix.Review); err != nil {
					logf("saving the answers: %v", err)
				}
				if code := j.runClaude(p, p.fixRole, step, model, fix, false); code != 0 {
					page(pageStopped)
					return end(code, "")
				}
				last.Fixed, last.FixCommit, last.Summary = true, gitx.Output(j.dir, "rev-parse", "HEAD"), j.lastResult.text
				addReplies(last, j.lastResult.text)
			} else { // re-run the step that asked
				page(pageRerunning)
				rerun := data
				rerun.Answers = renderAnswered(rounds)
				if code := j.runClaude(*p.asker, p.asker.name, step, askerModel, rerun, false); code != 0 {
					page(pageStopped)
					return end(code, "")
				}
				last.Rerun = true
			}
			if err := writeRound(dir, *last); err != nil {
				logf("saving the answers: %v", err)
			}
			continue
		}

		b, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) || (err == nil && strings.TrimSpace(string(b)) == "") {
			page(pageApproved)
			logf("[%s] %s: no question from %s", step, p.name, p.asker.name)
			return end(0, "")
		}
		if err != nil {
			logf("[%s] %s: reading the question: %v", step, p.name, err)
			return end(exitSetup, "")
		}
		question = string(b)
		if asked == p.maxRounds {
			page(pageStopped)
			logf("[%s] %s: stopped: %s asked again after %d rounds; the question is in %s, and prf resume %s/%s carries on", step, p.name, p.asker.name, asked, path, j.name, filepath.Base(j.jobDir))
			return end(exitAsk, " stopped=max_rounds")
		}
		asked++
		if srv == nil {
			if srv, err = startReviewServer(filepath.Join(j.jobDir, "reviews"), j.name, filepath.Base(j.jobDir), j.activity); err != nil {
				logf("starting the question page: %v", err)
				return end(exitSetup, "")
			}
			srv.fileAt = j.fileAtHead
		}

		view := j.askView(p, path, question, rounds, len(rounds)+1)
		srv.set(pageWaiting, view)
		j.setState(func(s *jobState) {
			s.State, s.Stage, s.Activity, s.ReviewURL = stateAsk, p.name, fmt.Sprintf("question %d", view.Round), srv.url
		})
		j.event("PRF_ASK branch=%s phase=%s step=%s round=%d url=%s", j.branch, p.name, step, view.Round, srv.url)
		logf("[%s] %s: %s has a question: waiting for your answers at %s", step, p.name, p.asker.name, srv.url)
		notify("prf: "+j.name+" has a question", fmt.Sprintf("%s, from %s", j.branch, p.asker.name))
		if j.reviewOpen && !srv.watched() {
			if err := openBrowser(srv.url); err != nil {
				logf("opening the question page: %v", err)
			}
		}

		var r reviewRound
		select {
		case r = <-srv.submitted:
		case <-j.sigs:
			logf("[%s] %s: interrupted while waiting for your answers", step, p.name)
			return end(130, "")
		}
		r.Round, r.Commit, r.Submitted, r.Question = view.Round, view.Head, time.Now(), question
		rounds = append(rounds, r)
		if err := writeRound(dir, r); err != nil {
			logf("saving the answers: %v", err)
		}
		j.setState(func(s *jobState) { s.State, s.Activity, s.ReviewURL = stateRunning, "", "" })
		j.event("PRF_ANSWERED branch=%s phase=%s step=%s round=%d verdict=%s comments=%d", j.branch, p.name, step, r.Round, r.Verdict, len(r.Comments))
		if r.Verdict == verdictStop {
			page(pageStopped)
			logf("[%s] %s: you stopped the job at question %d", step, p.name, r.Round)
			return end(exitOwner, " stopped=owner")
		}
		logf("[%s] %s: question %d answered (%s, %d comments)", step, p.name, r.Round, r.Verdict, len(r.Comments))
	}
}

// askView is the review page for an ask gate: the branch, and question.
func (j *job) askView(p phase, path, question string, rounds []reviewRound, round int) reviewView {
	v := j.reviewView(rounds, round)
	v.Kind, v.Question, v.QFile, v.Asker = pipeline.GateAsk, question, j.displayPath(path), p.asker.name
	return v
}

// renderAnswers is a round as the ask gate's fix role reads it, in {{.Review}}.
func renderAnswers(r reviewRound, path string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Round %d: the owner answered the question in `%s`.\n", r.Round, path)
	if r.Verdict == verdictAccept {
		fmt.Fprintf(&b, "\n%s\n", acceptText)
	}
	if r.Body != "" {
		fmt.Fprintf(&b, "\n%s\n", r.Body)
	}
	renderComments(&b, r.Comments)
	return b.String()
}

// renderAnswered is every answered question, for the step that asked them.
func renderAnswered(rounds []reviewRound) string {
	var b strings.Builder
	for _, r := range rounds {
		if !r.Fixed {
			continue
		}
		fmt.Fprintf(&b, "### Question %d\n\n%s\n\n### The owner's answer\n\n%s\n", r.Round, strings.TrimSpace(r.Question), renderAnswers(r, "the question file"))
	}
	return b.String()
}

// questionPath is where the gate looks for a question: q with environment
// variables expanded as the job's steps see them, relative to the checkout.
func (j *job) questionPath(q string) string {
	q = os.Expand(q, func(k string) string {
		v := os.Getenv(k)
		for _, kv := range j.env {
			if name, val, ok := strings.Cut(kv, "="); ok && name == k {
				v = val
			}
		}
		if v == "" && k == "TMPDIR" {
			v = os.TempDir()
		}
		return v
	})
	if !filepath.IsAbs(q) {
		q = filepath.Join(j.dir, q)
	}
	return filepath.Clean(q)
}

// displayPath is path relative to the checkout when it's inside it.
func (j *job) displayPath(path string) string {
	if rel, err := filepath.Rel(j.dir, path); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return path
}

// fileAtHead returns a file of the branch as committed at HEAD.
func (j *job) fileAtHead(path string) (string, bool) {
	path = filepath.ToSlash(filepath.Clean(path))
	if filepath.IsAbs(path) || path == ".." || strings.HasPrefix(path, "../") {
		return "", false
	}
	cmd := exec.Command("git", "show", "HEAD:"+path)
	cmd.Dir = j.dir
	b, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return string(b), true
}
