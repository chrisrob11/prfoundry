// Package dashboard is prf ui: a read-only page on localhost showing every
// job under a workdir, and one job's pipeline, spend, log, diff and gate
// rounds. It only reads job folders, so it shows jobs whoever started them.
package dashboard

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
	"time"

	"github.com/chrisrob11/prfoundry/internal/atomicfile"
)

//go:embed overview.html
var overviewPage []byte

//go:embed job.html
var jobPage []byte

//go:embed style.css
var stylesheet []byte

// LinkFile is <home>/ui.json: the dashboard's port and token, so its URL,
// and a bookmark of it, survive a restart.
const LinkFile = "ui.json"

// Options say which jobs the dashboard shows and where it listens.
type Options struct {
	Workdir string // where the job folders are: <workdir>/<repo>/<job>/
	Repo    string // only this repo's jobs; "" for every repo
	Port    int    // the port to listen on; 0 for the saved one if it's free, else any
	Link    string // where the port and token are kept; "" for a new token every time

	MaxDiff int // bytes of diff a job page shows; 0 for 512 KiB
	MaxLog  int // lines of claude.log a job page shows; 0 for 200

	Now  func() time.Time                 // the clock, for elapsed times; nil for time.Now
	Logf func(format string, args ...any) // for what goes wrong serving; nil to drop it
}

// Server serves the dashboard on 127.0.0.1 under a random token, so no
// other site or user on the machine can read the jobs.
type Server struct {
	o   Options
	url string
	srv *http.Server
	ln  net.Listener
}

// link is what Options.Link holds.
type link struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
}

// Start listens and serves the dashboard at http://127.0.0.1:<port>/<token>/.
// A port given in o that is taken is an error; a saved one that is taken
// is replaced.
func Start(o Options) (*Server, error) {
	if o.MaxDiff == 0 {
		o.MaxDiff = 512 << 10
	}
	if o.MaxLog == 0 {
		o.MaxLog = 200
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	var saved link
	if o.Link != "" {
		if b, err := os.ReadFile(o.Link); err == nil {
			if err := json.Unmarshal(b, &saved); err != nil {
				o.Logf("reading the dashboard's saved URL from %s: %v", o.Link, err)
			}
		}
	}
	ln, err := listen(o.Port, saved.Port)
	if err != nil {
		return nil, err
	}
	if saved.Token == "" {
		token := make([]byte, 16)
		_, _ = rand.Read(token) // crypto/rand.Read never fails
		saved.Token = hex.EncodeToString(token)
	}
	saved.Port = ln.Addr().(*net.TCPAddr).Port
	if o.Link != "" {
		b, _ := json.Marshal(saved)
		err := os.MkdirAll(filepath.Dir(o.Link), 0o755)
		if err == nil {
			err = atomicfile.Write(o.Link, append(b, '\n'))
		}
		if err != nil {
			o.Logf("saving the dashboard's URL: %v", err)
		}
	}
	prefix := "/" + saved.Token + "/"
	s := &Server{o: o, url: "http://" + ln.Addr().String() + prefix, ln: ln}
	s.srv = &http.Server{Handler: s.routes(prefix), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := s.srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			o.Logf("serving the dashboard: %v", err)
		}
	}()
	return s, nil
}

// listen binds 127.0.0.1 on port if it's set, else on saved if that's free,
// else on any free port.
func listen(port, saved int) (net.Listener, error) {
	if port > 0 {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			return nil, fmt.Errorf("port %d: %w", port, err)
		}
		return ln, nil
	}
	if saved > 0 {
		if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", saved)); err == nil {
			return ln, nil
		}
	}
	return net.Listen("tcp", "127.0.0.1:0")
}

// routes are the pages and the JSON API, all under prefix. Anything else,
// including every path without the token, is the mux's 404.
func (s *Server) routes(prefix string) http.Handler {
	mux := http.NewServeMux()
	page := func(body []byte, kind string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", kind)
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			_, _ = w.Write(body)
		}
	}
	html := "text/html; charset=utf-8"
	mux.HandleFunc("GET "+prefix+"{$}", page(overviewPage, html))
	mux.HandleFunc("GET "+prefix+"job/{repo}/{job}", page(jobPage, html))
	mux.HandleFunc("GET "+prefix+"style.css", page(stylesheet, "text/css; charset=utf-8"))
	mux.HandleFunc("GET "+prefix+"api/jobs", s.serveJobs)
	mux.HandleFunc("GET "+prefix+"api/jobs/{repo}/{job}", s.serveJob)
	mux.HandleFunc("GET "+prefix+"api/jobs/{repo}/{job}/log", s.serveLog)
	mux.HandleFunc("GET "+prefix+"api/jobs/{repo}/{job}/diff", s.serveDiff)
	mux.HandleFunc("GET "+prefix+"api/jobs/{repo}/{job}/checks/{step}/{round}", s.serveCheck)
	return mux
}

// URL is the dashboard's address, token included.
func (s *Server) URL() string {
	return s.url
}

// Close stops serving, giving open requests a second. ctx's cancellation,
// usually what stopped prf ui, doesn't cut that second short.
func (s *Server) Close(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	_ = s.srv.Shutdown(ctx) // open polls just drop
	_ = s.ln.Close()        // Serve may not have started, and then Shutdown can't see it
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v) // the status is sent; a failed write is the client gone
}
