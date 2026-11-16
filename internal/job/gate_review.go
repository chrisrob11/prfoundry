package job

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/chrisrob11/prfoundry/internal/gitx"
	"github.com/chrisrob11/prfoundry/internal/human"
	"github.com/chrisrob11/prfoundry/internal/pipeline"
	"github.com/chrisrob11/prfoundry/internal/review"
	"github.com/chrisrob11/prfoundry/internal/roles"
	"github.com/chrisrob11/prfoundry/internal/state"
	"github.com/chrisrob11/prfoundry/internal/textfile"
)

// A review gate (@review in a pipeline) stops the job and serves a review
// page for the branch, like a pull request's: its diff, where you comment on
// lines and then approve or request changes. Requested changes go to the
// gate's fix role, and the next round waits for you again, until you approve
// or max_rounds rounds of changes have run. Each round is saved in
// <job>/reviews/round-N.json, and the review as the fix role saw it in
// round-N.md.

// openBrowser opens url in your browser: Runtime.OpenBrowser's default.
func openBrowser(url string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	return exec.Command(name, url).Run()
}

// notify shows a desktop notification, on macOS: Runtime.Notify's default.
func notify(title, msg string) {
	if runtime.GOOS == "darwin" {
		go func() {
			_ = exec.Command("osascript", "-e", fmt.Sprintf("display notification %q with title %q sound name \"Glass\"", msg, title)).Run()
		}()
	}
}

// reviewGate runs the review gate p at step and returns its exit code: 0
// once you approve, the fix role's code if it fails, ExitReview after
// max_rounds rounds of changes, or 130 if interrupted while waiting. A
// resumed gate picks up the rounds in <job>/reviews, addressing a round
// of changes the job stopped before finishing.
func (j *Job) reviewGate(ctx context.Context, p phase, step, model string, data roles.Data) int {
	dir := filepath.Join(j.jobDir, "reviews")
	rounds, err := review.ReadRounds(dir)
	if err != nil {
		j.logf("%v", err)
		return ExitSetup
	}
	srv, err := j.startPage(nil)
	if err != nil {
		j.logf("starting the review page: %v", err)
		return ExitSetup
	}
	defer srv.Close(ctx)

	j.event("PRF_START branch=%s phase=%s step=%s pipeline=%s", j.branch, p.name, step, j.pipeline)
	start, fixes := time.Now(), 0
	end := func(code int, note string) int {
		j.event("PRF_PHASE branch=%s phase=%s step=%s exit=%d rounds=%d%s", j.branch, p.name, step, code, len(rounds), note)
		return code
	}
	for {
		if n := len(rounds); n > 0 && rounds[n-1].Verdict == review.Changes && !rounds[n-1].Fixed {
			last := &rounds[n-1]
			if fixes == p.maxRounds {
				srv.Set(review.PageStopped, j.reviewView(rounds, len(rounds)))
				j.logf("[%s] %s: stopped: changes still requested after %d rounds; prf resume %s/%s carries on", step, p.name, fixes, j.name, filepath.Base(j.jobDir))
				return end(ExitReview, " stopped=max_rounds")
			}
			fixes++
			srv.Set(review.PageFixing, j.reviewView(rounds, len(rounds)))
			data.Review = review.RenderChanges(*last)
			if err := textfile.Write(filepath.Join(dir, fmt.Sprintf("round-%d.md", last.Round)), data.Review); err != nil {
				j.logf("saving the review: %v", err)
			}
			if code := j.runClaude(p, p.fixRole, step, model, data, false); code != 0 {
				srv.Set(review.PageStopped, j.reviewView(rounds, len(rounds)))
				return end(code, "")
			}
			last.Fixed, last.FixCommit, last.Summary = true, gitx.Output(j.dir, "rev-parse", "HEAD"), j.lastResult.Text
			last.AddReplies(j.lastResult.Text)
			if err := review.WriteRound(dir, *last); err != nil {
				j.logf("saving the review: %v", err)
			}
			continue
		}

		view := j.reviewView(rounds, len(rounds)+1)
		srv.Set(review.PageWaiting, view)
		j.setState(func(s *state.Job) {
			s.State, s.Stage, s.Activity, s.ReviewURL = state.Review, p.name, fmt.Sprintf("round %d", view.Round), srv.URL()
		})
		j.event("PRF_REVIEW branch=%s phase=%s step=%s round=%d url=%s", j.branch, p.name, step, view.Round, srv.URL())
		j.logf("[%s] %s: round %d: waiting for your review at %s", step, p.name, view.Round, srv.URL())
		j.rt.Notify("prf: "+j.name+" is ready for review", fmt.Sprintf("%s, round %d", j.branch, view.Round))
		if j.reviewOpen && !srv.Watched() {
			if err := j.rt.OpenBrowser(srv.URL()); err != nil {
				j.logf("opening the review page: %v", err)
			}
		}

		var r review.Round
		select {
		case r = <-srv.Submitted():
		case <-j.sigs:
			j.logf("[%s] %s: interrupted while waiting for your review", step, p.name)
			return end(130, "")
		}
		r.Round, r.Commit, r.Submitted = view.Round, view.Head, time.Now()
		rounds = append(rounds, r)
		if err := review.WriteRound(dir, r); err != nil {
			j.logf("saving the review: %v", err)
		}
		j.setState(func(s *state.Job) { s.State, s.Activity, s.ReviewURL = state.Running, "", "" })
		j.event("PRF_REVIEWED branch=%s phase=%s step=%s round=%d verdict=%s comments=%d", j.branch, p.name, step, r.Round, r.Verdict, len(r.Comments))
		if r.Verdict == review.Approve {
			srv.Set(review.PageApproved, j.reviewView(rounds, len(rounds)))
			j.logf("[%s] %s: approved in round %d after %s", step, p.name, r.Round, human.Duration(time.Since(start)))
			return end(0, "")
		}
		j.logf("[%s] %s: round %d: changes requested (%d comments)", step, p.name, r.Round, len(r.Comments))
	}
}

// startPage serves the job's review page, which keeps its URL in
// <job>/reviews, with fileAt (nil for none) for the files it links to.
func (j *Job) startPage(fileAt func(path string) (string, bool)) (*review.Server, error) {
	return review.Start(review.Options{
		Dir: filepath.Join(j.jobDir, "reviews"), Repo: j.name, Job: filepath.Base(j.jobDir),
		Activity: j.activity, FileAt: fileAt, Logf: j.logf,
	})
}

// reviewView describes the branch as it is now, for round, after rounds.
func (j *Job) reviewView(rounds []review.Round, round int) review.View {
	head := gitx.Output(j.dir, "rev-parse", "HEAD")
	base := cmp.Or(j.state.BaseCommit, head)
	v := review.View{
		Kind: pipeline.GateReview, Repo: j.name, Job: filepath.Base(j.jobDir), Branch: j.branch, Pipeline: j.pipeline,
		Round: round, Base: base, Head: head, Task: j.task, Rounds: make([]review.Round, len(rounds)),
		Dirty: gitx.Output(j.dir, "status", "--porcelain") != "",
		Diff:  gitx.Output(j.dir, "diff", "--no-color", "--no-ext-diff", "-M", base, head),
	}
	for i, r := range rounds { // a copy: the gate goes on changing its rounds while the page reads this
		r.Comments = slices.Clone(r.Comments)
		v.Rounds[i] = r
	}
	if log := gitx.Output(j.dir, "log", "--format=%h %s", base+".."+head); log != "" {
		v.Commits = strings.Split(log, "\n")
	}
	if n := len(rounds); n > 0 {
		if since := rounds[n-1].Commit; since != "" && since != head {
			v.Since = since
			v.SinceDiff = gitx.Output(j.dir, "diff", "--no-color", "--no-ext-diff", "-M", since, head)
		}
	}
	if j.pr != "" {
		if b, err := os.ReadFile(j.pr); err == nil {
			v.Spec = string(b)
		}
	}
	return v
}
