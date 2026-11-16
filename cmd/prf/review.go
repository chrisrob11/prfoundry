package main

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A review gate (@review in a pipeline) stops the job and serves a review
// page for the branch, like a pull request's: its diff, where you comment on
// lines and then approve or request changes. Requested changes go to the
// gate's fix role, and the next round waits for you again, until you approve
// or max_rounds rounds of changes have run. Each round is saved in
// <job>/reviews/round-N.json, and the review as the fix role saw it in
// round-N.md.

// reviewPage is the single-page review UI.
//
//go:embed review.html
var reviewPage []byte

// Review verdicts, and an ask gate's.
const (
	verdictApprove = "approve"
	verdictChanges = "changes"
	verdictAnswer  = "answer" // your answers, in the body and comments
	verdictAccept  = "accept" // take every option marked Recommended
	verdictStop    = "stop"   // end the job
)

// What the review page shows: waiting for your review, Claude addressing
// it, or the gate over.
const (
	pageWaiting   = "waiting"
	pageSubmitted = "submitted"
	pageFixing    = "fixing"
	pageRerunning = "rerunning" // an ask gate re-running the step that asked
	pageApproved  = "approved"
	pageStopped   = "stopped"
)

type reviewComment struct {
	ID    int    `json:"id"`              // 1-based within the round; Claude replies to #id
	Path  string `json:"path,omitempty"`  // "" for none; the review's body holds general remarks
	Line  int    `json:"line,omitempty"`  // 0 for a comment on the whole file
	Side  string `json:"side,omitempty"`  // "new" or "old": which side of the diff Line is on
	Code  string `json:"code,omitempty"`  // the diff lines up to the commented one, or the question's block
	Block int    `json:"block,omitempty"` // the question's block (paragraph, heading, item) it's on, from 1
	Body  string `json:"body"`
	Reply string `json:"reply,omitempty"` // the fix role's answer
}

type reviewRound struct {
	Round     int             `json:"round"`
	Verdict   string          `json:"verdict"`
	Body      string          `json:"body,omitempty"`
	Comments  []reviewComment `json:"comments,omitempty"`
	Commit    string          `json:"commit"` // what was reviewed
	Submitted time.Time       `json:"submitted"`
	Fixed     bool            `json:"fixed,omitempty"` // the fix role has addressed it
	FixCommit string          `json:"fix_commit,omitempty"`
	Summary   string          `json:"summary,omitempty"`  // the fix role's final message
	Question  string          `json:"question,omitempty"` // an ask gate's: what you answered
	Rerun     bool            `json:"rerun,omitempty"`    // an ask gate's: the step that asked has run again
}

// replyLine is how the fix role answers a comment: "#3: renamed it".
var replyLine = regexp.MustCompile(`(?m)^[\s*-]*#(\d+):\s*(.+)$`)

// openBrowser and notify tell you a review is waiting; tests replace them.
var (
	openBrowser = func(url string) error {
		name := "xdg-open"
		if runtime.GOOS == "darwin" {
			name = "open"
		}
		return exec.Command(name, url).Run()
	}
	notify = func(title, msg string) {
		if runtime.GOOS == "darwin" {
			go func() {
				_ = exec.Command("osascript", "-e", fmt.Sprintf("display notification %q with title %q sound name \"Glass\"", msg, title)).Run()
			}()
		}
	}
)

// reviewGate runs the review gate p at step and returns its exit code: 0
// once you approve, the fix role's code if it fails, exitReview after
// max_rounds rounds of changes, or 130 if interrupted while waiting. A
// resumed gate picks up the rounds in <job>/reviews, addressing a round
// of changes the job stopped before finishing.
func (j *job) reviewGate(p phase, step, model string, data promptData) int {
	dir := filepath.Join(j.jobDir, "reviews")
	rounds, err := readRounds(dir)
	if err != nil {
		logf("%v", err)
		return exitSetup
	}
	srv, err := startReviewServer(dir, j.name, filepath.Base(j.jobDir), j.activity)
	if err != nil {
		logf("starting the review page: %v", err)
		return exitSetup
	}
	defer srv.close()

	j.event("PRF_START branch=%s phase=%s step=%s pipeline=%s", j.branch, p.name, step, j.pipeline)
	start, fixes := time.Now(), 0
	end := func(code int, note string) int {
		j.event("PRF_PHASE branch=%s phase=%s step=%s exit=%d rounds=%d%s", j.branch, p.name, step, code, len(rounds), note)
		return code
	}
	for {
		if n := len(rounds); n > 0 && rounds[n-1].Verdict == verdictChanges && !rounds[n-1].Fixed {
			last := &rounds[n-1]
			if fixes == p.maxRounds {
				srv.set(pageStopped, j.reviewView(rounds, len(rounds)))
				logf("[%s] %s: stopped: changes still requested after %d rounds; prf resume %s/%s carries on", step, p.name, fixes, j.name, filepath.Base(j.jobDir))
				return end(exitReview, " stopped=max_rounds")
			}
			fixes++
			srv.set(pageFixing, j.reviewView(rounds, len(rounds)))
			data.Review = renderReview(*last)
			if err := writeFile(filepath.Join(dir, fmt.Sprintf("round-%d.md", last.Round)), data.Review); err != nil {
				logf("saving the review: %v", err)
			}
			if code := j.runClaude(p, p.fixRole, step, model, data, false); code != 0 {
				srv.set(pageStopped, j.reviewView(rounds, len(rounds)))
				return end(code, "")
			}
			last.Fixed, last.FixCommit, last.Summary = true, gitOutput(j.dir, "rev-parse", "HEAD"), j.lastResult.text
			addReplies(last, j.lastResult.text)
			if err := writeRound(dir, *last); err != nil {
				logf("saving the review: %v", err)
			}
			continue
		}

		view := j.reviewView(rounds, len(rounds)+1)
		srv.set(pageWaiting, view)
		j.setState(func(s *jobState) {
			s.State, s.Stage, s.Activity, s.ReviewURL = stateReview, p.name, fmt.Sprintf("round %d", view.Round), srv.url
		})
		j.event("PRF_REVIEW branch=%s phase=%s step=%s round=%d url=%s", j.branch, p.name, step, view.Round, srv.url)
		logf("[%s] %s: round %d: waiting for your review at %s", step, p.name, view.Round, srv.url)
		notify("prf: "+j.name+" is ready for review", fmt.Sprintf("%s, round %d", j.branch, view.Round))
		if j.reviewOpen && !srv.watched() {
			if err := openBrowser(srv.url); err != nil {
				logf("opening the review page: %v", err)
			}
		}

		var r reviewRound
		select {
		case r = <-srv.submitted:
		case <-j.sigs:
			logf("[%s] %s: interrupted while waiting for your review", step, p.name)
			return end(130, "")
		}
		r.Round, r.Commit, r.Submitted = view.Round, view.Head, time.Now()
		rounds = append(rounds, r)
		if err := writeRound(dir, r); err != nil {
			logf("saving the review: %v", err)
		}
		j.setState(func(s *jobState) { s.State, s.Activity, s.ReviewURL = stateRunning, "", "" })
		j.event("PRF_REVIEWED branch=%s phase=%s step=%s round=%d verdict=%s comments=%d", j.branch, p.name, step, r.Round, r.Verdict, len(r.Comments))
		if r.Verdict == verdictApprove {
			srv.set(pageApproved, j.reviewView(rounds, len(rounds)))
			logf("[%s] %s: approved in round %d after %s", step, p.name, r.Round, shortDuration(time.Since(start)))
			return end(0, "")
		}
		logf("[%s] %s: round %d: changes requested (%d comments)", step, p.name, r.Round, len(r.Comments))
	}
}

// activity is Claude's latest step, for the review page while it fixes.
func (j *job) activity() string {
	j.stateMu.Lock()
	defer j.stateMu.Unlock()
	return j.state.Activity
}

// reviewView is everything the review page shows.
type reviewView struct {
	Version   int           `json:"version"`
	Kind      string        `json:"kind"` // gateReview or gateAsk
	State     string        `json:"state"`
	Activity  string        `json:"activity,omitempty"`
	Repo      string        `json:"repo"`
	Job       string        `json:"job"`
	Branch    string        `json:"branch"`
	Pipeline  string        `json:"pipeline"`
	Round     int           `json:"round"` // the round waiting for review, or the one being fixed
	Base      string        `json:"base"`
	Head      string        `json:"head"`
	Dirty     bool          `json:"dirty"` // the checkout has uncommitted changes, which the diff leaves out
	Commits   []string      `json:"commits"`
	Diff      string        `json:"diff"`
	Since     string        `json:"since,omitempty"` // the last reviewed commit, when the branch has moved since
	SinceDiff string        `json:"since_diff,omitempty"`
	Spec      string        `json:"spec,omitempty"`
	Task      string        `json:"task,omitempty"`
	Rounds    []reviewRound `json:"rounds"`
	Question  string        `json:"question,omitempty"`      // an ask gate's question
	QFile     string        `json:"question_file,omitempty"` // and where it is
	Asker     string        `json:"asker,omitempty"`         // the step that asked
}

// reviewView describes the branch as it is now, for round, after rounds.
func (j *job) reviewView(rounds []reviewRound, round int) reviewView {
	head := gitOutput(j.dir, "rev-parse", "HEAD")
	base := firstNonEmpty(j.state.BaseCommit, head)
	v := reviewView{
		Kind: gateReview, Repo: j.name, Job: filepath.Base(j.jobDir), Branch: j.branch, Pipeline: j.pipeline,
		Round: round, Base: base, Head: head, Task: j.task, Rounds: make([]reviewRound, len(rounds)),
		Dirty: gitOutput(j.dir, "status", "--porcelain") != "",
		Diff:  gitOutput(j.dir, "diff", "--no-color", "--no-ext-diff", "-M", base, head),
	}
	for i, r := range rounds { // a copy: the gate goes on changing its rounds while the page reads this
		r.Comments = slices.Clone(r.Comments)
		v.Rounds[i] = r
	}
	if log := gitOutput(j.dir, "log", "--format=%h %s", base+".."+head); log != "" {
		v.Commits = strings.Split(log, "\n")
	}
	if n := len(rounds); n > 0 {
		if since := rounds[n-1].Commit; since != "" && since != head {
			v.Since = since
			v.SinceDiff = gitOutput(j.dir, "diff", "--no-color", "--no-ext-diff", "-M", since, head)
		}
	}
	if j.pr != "" {
		if b, err := os.ReadFile(j.pr); err == nil {
			v.Spec = string(b)
		}
	}
	return v
}

// renderReview is a round as its fix role reads it, in {{.Review}}.
func renderReview(r reviewRound) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Round %d: the reviewer requested changes.\n", r.Round)
	if r.Body != "" {
		fmt.Fprintf(&b, "\n%s\n", r.Body)
	}
	renderComments(&b, r.Comments)
	return b.String()
}

// renderComments writes each comment under a "### #id: where" heading.
func renderComments(b *strings.Builder, comments []reviewComment) {
	for _, c := range comments {
		where := "general"
		switch {
		case c.Block > 0:
			where = "on the question"
		case c.Path != "" && c.Line > 0:
			where = fmt.Sprintf("`%s` line %d", c.Path, c.Line)
			if c.Side == "old" {
				where += " (a removed line)"
			}
		case c.Path != "":
			where = fmt.Sprintf("`%s`", c.Path)
		}
		fmt.Fprintf(b, "\n### #%d: %s\n\n", c.ID, where)
		switch {
		case c.Code != "" && c.Block > 0:
			fmt.Fprintf(b, "> %s\n\n", strings.ReplaceAll(strings.TrimRight(c.Code, "\n"), "\n", "\n> "))
		case c.Code != "":
			fmt.Fprintf(b, "```diff\n%s\n```\n\n", strings.TrimRight(c.Code, "\n"))
		}
		fmt.Fprintf(b, "%s\n", c.Body)
	}
}

// addReplies records the fix role's "#id: reply" lines on r's comments.
func addReplies(r *reviewRound, text string) {
	for _, m := range replyLine.FindAllStringSubmatch(text, -1) {
		id, _ := strconv.Atoi(m[1])
		if i := slices.IndexFunc(r.Comments, func(c reviewComment) bool { return c.ID == id }); i >= 0 {
			r.Comments[i].Reply = strings.TrimSpace(m[2])
		}
	}
}

// readRounds returns the rounds saved in dir, in order. A missing dir has none.
func readRounds(dir string) ([]reviewRound, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "round-*.json"))
	if err != nil {
		return nil, err
	}
	var rounds []reviewRound
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var r reviewRound
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		rounds = append(rounds, r)
	}
	slices.SortFunc(rounds, func(a, b reviewRound) int { return a.Round - b.Round })
	return rounds, nil
}

func writeRound(dir string, r reviewRound) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, fmt.Sprintf("round-%d.json", r.Round)), append(b, '\n'))
}

// reviewServer serves the review page on localhost for one gate, under a
// random path so no other site or user can post a review.
type reviewServer struct {
	url       string
	srv       *http.Server
	ln        net.Listener
	submitted chan reviewRound
	activity  func() string
	fileAt    func(path string) (string, bool) // a file at HEAD, for links the page can't find in the diff

	mu     sync.Mutex
	view   reviewView
	polled time.Time // when the page last asked for news
}

// reviewLink is <job>/reviews/link.json: the port and token of the job's
// review page, so the page keeps its URL when the job is resumed.
type reviewLink struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
}

// unsafePathChars are what a job's name can't keep in its review URL.
var unsafePathChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// startReviewServer serves the review page of job (in repo) at
// http://127.0.0.1:<port>/<repo>/<job>/<token>/, reusing the port and token
// saved in dir, unless the port is now taken.
func startReviewServer(dir, repo, job string, activity func() string) (*reviewServer, error) {
	var link reviewLink
	path := filepath.Join(dir, "link.json")
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &link); err != nil {
			logf("reading the review page's saved URL from %s: %v", path, err)
		}
	}
	var ln net.Listener
	if link.Port > 0 {
		ln, _ = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", link.Port))
	}
	if ln == nil {
		var err error
		if ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			return nil, err
		}
		if link.Port > 0 {
			logf("port %d is taken, so the review page has a new URL", link.Port)
		}
	}
	if link.Token == "" {
		token := make([]byte, 16)
		rand.Read(token)
		link.Token = hex.EncodeToString(token)
	}
	link.Port = ln.Addr().(*net.TCPAddr).Port
	b, _ := json.Marshal(link)
	err := os.MkdirAll(dir, 0o755)
	if err == nil {
		err = writeAtomic(path, append(b, '\n'))
	}
	if err != nil {
		logf("saving the review page's URL: %v", err)
	}
	prefix := "/" + repo + "/" + unsafePathChars.ReplaceAllString(job, "-") + "/" + link.Token + "/"
	s := &reviewServer{
		url:       "http://" + ln.Addr().String() + prefix,
		ln:        ln,
		submitted: make(chan reviewRound, 1),
		activity:  activity,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+prefix+"{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(reviewPage)
	})
	mux.HandleFunc("GET "+prefix+"api/state", s.serveState)
	mux.HandleFunc("GET "+prefix+"api/review", s.serveReview)
	mux.HandleFunc("POST "+prefix+"api/submit", s.serveSubmit)
	mux.HandleFunc("GET "+prefix+"api/file", s.serveFile)
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := s.srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			logf("serving the review page: %v", err)
		}
	}()
	return s, nil
}

func (s *reviewServer) close() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = s.srv.Shutdown(ctx) // the job is done with the page; open connections just drop
	// Serve may not have started yet, and then Shutdown cannot see the listener:
	// close it here so the port is free when close returns.
	_ = s.ln.Close()
}

// set moves the page to state, showing view.
func (s *reviewServer) set(state string, view reviewView) {
	s.mu.Lock()
	defer s.mu.Unlock()
	view.State, view.Version = state, s.view.Version+1
	s.view = view
}

// watched reports whether the page is open: it polls every two seconds.
func (s *reviewServer) watched() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.polled) < 6*time.Second
}

func (s *reviewServer) serveState(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.polled = time.Now()
	state := map[string]any{"version": s.view.Version, "state": s.view.State, "round": s.view.Round}
	s.mu.Unlock()
	if state["state"] == pageFixing || state["state"] == pageRerunning {
		state["activity"] = s.activity()
	}
	writeHTTPJSON(w, http.StatusOK, state)
}

func (s *reviewServer) serveReview(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.polled = time.Now()
	view := s.view
	s.mu.Unlock()
	writeHTTPJSON(w, http.StatusOK, view)
}

func (s *reviewServer) serveSubmit(w http.ResponseWriter, r *http.Request) {
	var sub reviewRound
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&sub); err != nil {
		writeHTTPJSON(w, http.StatusBadRequest, map[string]string{"error": "bad review: " + err.Error()})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := cleanSubmission(&sub, s.view.Kind); err != nil {
		writeHTTPJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if s.view.State != pageWaiting {
		writeHTTPJSON(w, http.StatusConflict, map[string]string{"error": "this round isn't waiting for a review"})
		return
	}
	s.view.State, s.view.Version = pageSubmitted, s.view.Version+1
	s.submitted <- sub
	writeHTTPJSON(w, http.StatusOK, map[string]any{"version": s.view.Version})
}

// cleanSubmission checks a posted review, or answers to an ask gate's
// question (kind), drops empty comments, and numbers the rest.
func cleanSubmission(r *reviewRound, kind string) error {
	if kind == gateAsk {
		if r.Verdict != verdictAnswer && r.Verdict != verdictAccept && r.Verdict != verdictStop {
			return fmt.Errorf("verdict must be %q, %q or %q", verdictAnswer, verdictAccept, verdictStop)
		}
	} else if r.Verdict != verdictApprove && r.Verdict != verdictChanges {
		return fmt.Errorf("verdict must be %q or %q", verdictApprove, verdictChanges)
	}
	r.Body = strings.TrimSpace(r.Body)
	var comments []reviewComment
	for _, c := range r.Comments {
		c.Body = strings.TrimSpace(c.Body)
		if c.Body == "" {
			continue
		}
		c.ID, c.Reply = len(comments)+1, ""
		comments = append(comments, c)
	}
	r.Comments = comments
	if r.Verdict == verdictChanges && r.Body == "" && len(comments) == 0 {
		return errors.New("say what to change: add a comment or a summary")
	}
	if r.Verdict == verdictAnswer && r.Body == "" && len(comments) == 0 {
		return errors.New("write an answer, or accept the recommendations")
	}
	return nil
}

// serveFile returns a file of the branch at HEAD, as {"path", "text"}.
func (s *reviewServer) serveFile(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if s.fileAt == nil || path == "" {
		writeHTTPJSON(w, http.StatusNotFound, map[string]string{"error": "no such file"})
		return
	}
	text, ok := s.fileAt(path)
	if !ok {
		writeHTTPJSON(w, http.StatusNotFound, map[string]string{"error": path + " isn't in the branch"})
		return
	}
	writeHTTPJSON(w, http.StatusOK, map[string]string{"path": path, "text": text})
}

func writeHTTPJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v) // the status is sent; a failed write is the client gone
}
