package job

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/chrisrob11/prfoundry/internal/gitx"
	"github.com/chrisrob11/prfoundry/internal/pipeline"
	"github.com/chrisrob11/prfoundry/internal/review"
	"github.com/chrisrob11/prfoundry/internal/roles"
	"github.com/chrisrob11/prfoundry/internal/state"
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

// askGate runs the ask gate p at step and returns its exit code: 0 once the
// step before it finishes without a question, the fix role's or the step's
// code if either fails, ExitAsk when the step asks more than max_rounds
// times, ExitOwner if you stop the job, or 130 if interrupted while waiting.
// A resumed gate picks up where its saved rounds left off.
func (j *Job) askGate(ctx context.Context, p phase, step, model string, data roles.Data) int {
	n, _, _ := strings.Cut(step, "/")
	dir := filepath.Join(j.jobDir, "asks", "step-"+n)
	rounds, err := review.ReadRounds(dir)
	if err != nil {
		j.logf("%v", err)
		return ExitSetup
	}
	path := j.questionPath(p.question)
	askerModel := cmp.Or(j.model, p.asker.model, j.defaultModel)

	j.event("PRF_START branch=%s phase=%s step=%s pipeline=%s", j.branch, p.name, step, j.pipeline)
	var srv *review.Server // started at the first question
	defer func() {
		if srv != nil {
			srv.Close(ctx)
		}
	}()
	question := ""
	page := func(state string) {
		if srv != nil {
			srv.Set(state, j.askView(p, path, question, rounds, len(rounds)))
		}
	}
	asked := 0
	end := func(code int, note string) int {
		j.event("PRF_PHASE branch=%s phase=%s step=%s exit=%d rounds=%d%s", j.branch, p.name, step, code, len(rounds), note)
		return code
	}
	for {
		if n := len(rounds); n > 0 && rounds[n-1].Verdict != review.Stop && !rounds[n-1].Rerun {
			last := &rounds[n-1]
			if !last.Fixed { // record your answers
				page(review.PageFixing)
				fix := data
				fix.Question, fix.Review = last.Question, review.RenderAnswers(*last, j.displayPath(path))
				if err := textfile.Write(filepath.Join(dir, fmt.Sprintf("round-%d.md", last.Round)), fix.Review); err != nil {
					j.logf("saving the answers: %v", err)
				}
				if code := j.runClaude(p, p.fixRole, step, model, fix, false); code != 0 {
					page(review.PageStopped)
					return end(code, "")
				}
				last.Fixed, last.FixCommit, last.Summary = true, gitx.Output(j.dir, "rev-parse", "HEAD"), j.lastResult.Text
				last.AddReplies(j.lastResult.Text)
			} else { // re-run the step that asked
				page(review.PageRerunning)
				rerun := data
				rerun.Answers = review.RenderAnswered(rounds)
				if code := j.runClaude(*p.asker, p.asker.name, step, askerModel, rerun, false); code != 0 {
					page(review.PageStopped)
					return end(code, "")
				}
				last.Rerun = true
			}
			if err := review.WriteRound(dir, *last); err != nil {
				j.logf("saving the answers: %v", err)
			}
			continue
		}

		b, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) || (err == nil && strings.TrimSpace(string(b)) == "") {
			page(review.PageApproved)
			j.logf("[%s] %s: no question from %s", step, p.name, p.asker.name)
			return end(0, "")
		}
		if err != nil {
			j.logf("[%s] %s: reading the question: %v", step, p.name, err)
			return end(ExitSetup, "")
		}
		question = string(b)
		if asked == p.maxRounds {
			page(review.PageStopped)
			j.logf("[%s] %s: stopped: %s asked again after %d rounds; the question is in %s, and prf resume %s/%s carries on", step, p.name, p.asker.name, asked, path, j.name, filepath.Base(j.jobDir))
			return end(ExitAsk, " stopped=max_rounds")
		}
		asked++
		if srv == nil {
			if srv, err = j.startPage(j.fileAtHead); err != nil {
				j.logf("starting the question page: %v", err)
				return end(ExitSetup, "")
			}
		}

		view := j.askView(p, path, question, rounds, len(rounds)+1)
		r, ok := j.awaitRound(srv, view, dir, p, step, pageWait{
			state: state.Ask, activity: fmt.Sprintf("question %d", view.Round), event: "PRF_ASK", done: "PRF_ANSWERED",
			why: p.asker.name + " has a question", what: "your answers", page: "question page", saved: "answers",
			title: "prf: " + j.name + " has a question", body: fmt.Sprintf("%s, from %s", j.branch, p.asker.name),
		})
		if !ok {
			return end(130, "")
		}
		rounds = append(rounds, r)
		if r.Verdict == review.Stop {
			page(review.PageStopped)
			j.logf("[%s] %s: you stopped the job at question %d", step, p.name, r.Round)
			return end(ExitOwner, " stopped=owner")
		}
		j.logf("[%s] %s: question %d answered (%s, %d comments)", step, p.name, r.Round, r.Verdict, len(r.Comments))
	}
}

// askView is the review page for an ask gate: the branch, and question.
func (j *Job) askView(p phase, path, question string, rounds []review.Round, round int) review.View {
	v := j.reviewView(rounds, round)
	v.Kind, v.Question, v.QFile, v.Asker = pipeline.GateAsk, question, j.displayPath(path), p.asker.name
	return v
}

// questionPath is where the gate looks for a question: q with environment
// variables expanded as the job's steps see them, relative to the checkout.
func (j *Job) questionPath(q string) string {
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
func (j *Job) displayPath(path string) string {
	if rel, err := filepath.Rel(j.dir, path); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return path
}

// fileAtHead returns a file of the branch as committed at HEAD.
func (j *Job) fileAtHead(path string) (string, bool) {
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
