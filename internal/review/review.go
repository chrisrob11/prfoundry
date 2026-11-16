// Package review is the page a job serves on localhost at a @review or
// @ask gate, like a pull request's: the branch's diff, where you comment on
// lines and approve or request changes, or answer the question a step left.
// It also saves each round in round-N.json, and renders a round as the
// gate's fix role reads it.
package review

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
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chrisrob11/prfoundry/internal/atomicfile"
	"github.com/chrisrob11/prfoundry/internal/pipeline"
)

// reviewPage is the single-page review UI.
//
//go:embed review.html
var reviewPage []byte

// Review verdicts, and an ask gate's.
const (
	Approve = "approve"
	Changes = "changes"
	Answer  = "answer" // your answers, in the body and comments
	accept  = "accept" // take every option marked Recommended
	Stop    = "stop"   // end the job
)

// What the review page shows: waiting for your review, Claude addressing
// it, or the gate over.
const (
	PageWaiting   = "waiting"
	pageSubmitted = "submitted"
	PageFixing    = "fixing"
	PageRerunning = "rerunning" // an ask gate re-running the step that asked
	PageApproved  = "approved"
	PageStopped   = "stopped"
)

// Comment is a comment on a round: on a line of the diff, a file, the
// question, or none of them.
type Comment struct {
	ID    int    `json:"id"`              // 1-based within the round; Claude replies to #id
	Path  string `json:"path,omitempty"`  // "" for none; the review's body holds general remarks
	Line  int    `json:"line,omitempty"`  // 0 for a comment on the whole file
	Side  string `json:"side,omitempty"`  // "new" or "old": which side of the diff Line is on
	Code  string `json:"code,omitempty"`  // the diff lines up to the commented one, or the question's block
	Block int    `json:"block,omitempty"` // the question's block (paragraph, heading, item) it's on, from 1
	Body  string `json:"body"`
	Reply string `json:"reply,omitempty"` // the fix role's answer
}

// Round is one review, or one set of answers to an ask gate's question,
// and what the fix role made of it.
type Round struct {
	Round     int       `json:"round"`
	Verdict   string    `json:"verdict"`
	Body      string    `json:"body,omitempty"`
	Comments  []Comment `json:"comments,omitempty"`
	Commit    string    `json:"commit"` // what was reviewed
	Submitted time.Time `json:"submitted"`
	Fixed     bool      `json:"fixed,omitempty"` // the fix role has addressed it
	FixCommit string    `json:"fix_commit,omitempty"`
	Summary   string    `json:"summary,omitempty"`  // the fix role's final message
	Question  string    `json:"question,omitempty"` // an ask gate's: what you answered
	Rerun     bool      `json:"rerun,omitempty"`    // an ask gate's: the step that asked has run again
}

// replyLine is how the fix role answers a comment: "#3: renamed it".
var replyLine = regexp.MustCompile(`(?m)^[\s*-]*#(\d+):\s*(.+)$`)

// View is everything the review page shows.
type View struct {
	Version   int      `json:"version"`
	Kind      string   `json:"kind"` // pipeline.GateReview or GateAsk
	State     string   `json:"state"`
	Activity  string   `json:"activity,omitempty"`
	Repo      string   `json:"repo"`
	Job       string   `json:"job"`
	Branch    string   `json:"branch"`
	Pipeline  string   `json:"pipeline"`
	Round     int      `json:"round"` // the round waiting for review, or the one being fixed
	Base      string   `json:"base"`
	Head      string   `json:"head"`
	Dirty     bool     `json:"dirty"` // the checkout has uncommitted changes, which the diff leaves out
	Commits   []string `json:"commits"`
	Diff      string   `json:"diff"`
	Since     string   `json:"since,omitempty"` // the last reviewed commit, when the branch has moved since
	SinceDiff string   `json:"since_diff,omitempty"`
	Spec      string   `json:"spec,omitempty"`
	Task      string   `json:"task,omitempty"`
	Rounds    []Round  `json:"rounds"`
	Question  string   `json:"question,omitempty"`      // an ask gate's question
	QFile     string   `json:"question_file,omitempty"` // and where it is
	Asker     string   `json:"asker,omitempty"`         // the step that asked
}

// RenderChanges is a round of requested changes as its fix role reads it,
// in {{.Review}}.
func RenderChanges(r Round) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Round %d: the reviewer requested changes.\n", r.Round)
	if r.Body != "" {
		fmt.Fprintf(&b, "\n%s\n", r.Body)
	}
	renderComments(&b, r.Comments)
	return b.String()
}

// renderComments writes each comment under a "### #id: where" heading.
func renderComments(b *strings.Builder, comments []Comment) {
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

// AddReplies records the fix role's "#id: reply" lines on r's comments.
func (r *Round) AddReplies(text string) {
	for _, m := range replyLine.FindAllStringSubmatch(text, -1) {
		id, _ := strconv.Atoi(m[1])
		if i := slices.IndexFunc(r.Comments, func(c Comment) bool { return c.ID == id }); i >= 0 {
			r.Comments[i].Reply = strings.TrimSpace(m[2])
		}
	}
}

// ReadRounds returns the rounds saved in dir, in order. A missing dir has none.
func ReadRounds(dir string) ([]Round, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "round-*.json"))
	if err != nil {
		return nil, err
	}
	var rounds []Round
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var r Round
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		rounds = append(rounds, r)
	}
	slices.SortFunc(rounds, func(a, b Round) int { return a.Round - b.Round })
	return rounds, nil
}

// WriteRound saves r in dir as round-N.json.
func WriteRound(dir string, r Round) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return atomicfile.Write(filepath.Join(dir, fmt.Sprintf("round-%d.json", r.Round)), append(b, '\n'))
}

// AcceptText is what "Accept recommendations" answers.
const AcceptText = "The owner accepts the option marked Recommended for every question."

// RenderAnswers is a round as the ask gate's fix role reads it, in {{.Review}}.
func RenderAnswers(r Round, path string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Round %d: the owner answered the question in `%s`.\n", r.Round, path)
	if r.Verdict == accept {
		fmt.Fprintf(&b, "\n%s\n", AcceptText)
	}
	if r.Body != "" {
		fmt.Fprintf(&b, "\n%s\n", r.Body)
	}
	renderComments(&b, r.Comments)
	return b.String()
}

// RenderAnswered is every answered question, for the step that asked them.
func RenderAnswered(rounds []Round) string {
	var b strings.Builder
	for _, r := range rounds {
		if !r.Fixed {
			continue
		}
		fmt.Fprintf(&b, "### Question %d\n\n%s\n\n### The owner's answer\n\n%s\n", r.Round, strings.TrimSpace(r.Question), RenderAnswers(r, "the question file"))
	}
	return b.String()
}

// Server serves the review page on localhost for one gate, under a
// random path so no other site or user can post a review.
type Server struct {
	url       string
	srv       *http.Server
	ln        net.Listener
	submitted chan Round
	activity  func() string
	fileAt    func(path string) (string, bool)

	mu     sync.Mutex
	view   View
	polled time.Time // when the page last asked for news
}

// link is <job>/reviews/link.json: the port and token of the job's
// review page, so the page keeps its URL when the job is resumed.
type link struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
}

// unsafePathChars are what a job's name can't keep in its review URL.
var unsafePathChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// Options say which job's page a Server serves, and how it reaches the job.
type Options struct {
	Dir      string // <job>/reviews: where link.json is kept
	Repo     string
	Job      string
	Activity func() string                    // Claude's latest step, while it fixes
	FileAt   func(path string) (string, bool) // a file at HEAD, for links the page can't find in the diff; nil for none
	Logf     func(format string, args ...any) // for what goes wrong serving the page
}

// Start serves the review page of o.Job (in o.Repo) at
// http://127.0.0.1:<port>/<repo>/<job>/<token>/, reusing the port and token
// saved in o.Dir, unless the port is now taken.
func Start(o Options) (*Server, error) {
	var link link
	dir, logf := o.Dir, o.Logf
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
		err = atomicfile.Write(path, append(b, '\n'))
	}
	if err != nil {
		logf("saving the review page's URL: %v", err)
	}
	prefix := "/" + o.Repo + "/" + unsafePathChars.ReplaceAllString(o.Job, "-") + "/" + link.Token + "/"
	s := &Server{
		url:       "http://" + ln.Addr().String() + prefix,
		ln:        ln,
		submitted: make(chan Round, 1),
		activity:  o.Activity,
		fileAt:    o.FileAt,
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

// URL is the page's address.
func (s *Server) URL() string {
	return s.url
}

// Submitted delivers each review, or set of answers, posted on the page.
func (s *Server) Submitted() <-chan Round {
	return s.submitted
}

// Close stops serving the page, giving open requests a second. ctx's
// cancellation, usually what ended the job, doesn't cut that second short.
func (s *Server) Close(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	_ = s.srv.Shutdown(ctx) // the job is done with the page; open connections just drop
	// Serve may not have started yet, and then Shutdown cannot see the listener:
	// close it here so the port is free when close returns.
	_ = s.ln.Close()
}

// Set moves the page to state, showing view.
func (s *Server) Set(state string, view View) {
	s.mu.Lock()
	defer s.mu.Unlock()
	view.State, view.Version = state, s.view.Version+1
	s.view = view
}

// Watched reports whether the page is open: it polls every two seconds.
func (s *Server) Watched() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.polled) < 6*time.Second
}

func (s *Server) serveState(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.polled = time.Now()
	state := map[string]any{"version": s.view.Version, "state": s.view.State, "round": s.view.Round}
	s.mu.Unlock()
	if state["state"] == PageFixing || state["state"] == PageRerunning {
		state["activity"] = s.activity()
	}
	writeHTTPJSON(w, http.StatusOK, state)
}

func (s *Server) serveReview(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.polled = time.Now()
	view := s.view
	s.mu.Unlock()
	writeHTTPJSON(w, http.StatusOK, view)
}

func (s *Server) serveSubmit(w http.ResponseWriter, r *http.Request) {
	var sub Round
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
	if s.view.State != PageWaiting {
		writeHTTPJSON(w, http.StatusConflict, map[string]string{"error": "this round isn't waiting for a review"})
		return
	}
	s.view.State, s.view.Version = pageSubmitted, s.view.Version+1
	s.submitted <- sub
	writeHTTPJSON(w, http.StatusOK, map[string]any{"version": s.view.Version})
}

// cleanSubmission checks a posted review, or answers to an ask gate's
// question (kind), drops empty comments, and numbers the rest.
func cleanSubmission(r *Round, kind string) error {
	if kind == pipeline.GateAsk {
		if r.Verdict != Answer && r.Verdict != accept && r.Verdict != Stop {
			return fmt.Errorf("verdict must be %q, %q or %q", Answer, accept, Stop)
		}
	} else if r.Verdict != Approve && r.Verdict != Changes {
		return fmt.Errorf("verdict must be %q or %q", Approve, Changes)
	}
	r.Body = strings.TrimSpace(r.Body)
	var comments []Comment
	for _, c := range r.Comments {
		c.Body = strings.TrimSpace(c.Body)
		if c.Body == "" {
			continue
		}
		c.ID, c.Reply = len(comments)+1, ""
		comments = append(comments, c)
	}
	r.Comments = comments
	if r.Verdict == Changes && r.Body == "" && len(comments) == 0 {
		return errors.New("say what to change: add a comment or a summary")
	}
	if r.Verdict == Answer && r.Body == "" && len(comments) == 0 {
		return errors.New("write an answer, or accept the recommendations")
	}
	return nil
}

// serveFile returns a file of the branch at HEAD, as {"path", "text"}.
func (s *Server) serveFile(w http.ResponseWriter, r *http.Request) {
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
