package dashboard

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chrisrob11/prfoundry/internal/state"
)

// now is the clock every test's dashboard reads.
var now = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

// start serves the dashboard of workdir with o's other settings, closing
// it when the test ends.
func start(t *testing.T, o Options) *Server {
	t.Helper()
	if o.Now == nil {
		o.Now = func() time.Time { return now }
	}
	o.Logf = t.Logf
	srv, err := Start(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	return srv
}

// get fetches url, returning its status, body and content type.
func get(t *testing.T, url string) (int, string, string) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, string(b), res.Header.Get("Content-Type")
}

// getJSON fetches url into v, failing unless it's a 200.
func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	code, body, _ := get(t, url)
	if code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", url, code, body)
	}
	if err := json.Unmarshal([]byte(body), v); err != nil {
		t.Fatalf("GET %s: %v\n%s", url, err, body)
	}
}

func writeFile(t *testing.T, path, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeJob writes j's state.json in workdir, returning its job folder.
func writeJob(t *testing.T, workdir string, j state.Job) string {
	t.Helper()
	dir := filepath.Join(workdir, j.Repo, j.Job)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := state.Write(dir, j); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestEveryPathNeedsTheToken: a request without the token, or with
// another, gets a 404 from every route, so a page on another site, or
// another user on the machine, can't read a job.
func TestEveryPathNeedsTheToken(t *testing.T) {
	workdir := t.TempDir()
	writeJob(t, workdir, state.Job{Repo: "app", Job: "a", State: state.Done})
	writeFile(t, filepath.Join(workdir, "app", "a", "claude.log"), "secret\n")
	writeFile(t, filepath.Join(workdir, "app", "a", "checks", "step-1", "round-1.log"), "secret\n")
	srv := start(t, Options{Workdir: workdir})
	host := "http://" + strings.Split(srv.URL(), "/")[2]
	routes := []string{"", "style.css", "job/app/a", "api/jobs", "api/jobs/app/a", "api/jobs/app/a/log", "api/jobs/app/a/diff", "api/jobs/app/a/checks/1/1"}
	for _, route := range routes {
		if code, _, _ := get(t, srv.URL()+route); code != http.StatusOK {
			t.Errorf("GET %s with the token: %d, want 200", route, code)
		}
		for _, url := range []string{host + "/" + route, host + "/0123456789abcdef0123456789abcdef/" + route} {
			if code, body, _ := get(t, url); code != http.StatusNotFound || strings.Contains(body, "secret") {
				t.Errorf("GET %s: %d %q, want 404", url, code, body)
			}
		}
	}
	if !strings.HasPrefix(srv.URL(), "http://127.0.0.1:") {
		t.Errorf("url %s isn't on 127.0.0.1", srv.URL())
	}
}

// TestTheURLIsStable: the token and port are saved, so a restart, or a
// bookmark, keeps the URL; a --port that's taken is an error.
func TestTheURLIsStable(t *testing.T) {
	link := filepath.Join(t.TempDir(), "home", LinkFile)
	first, err := Start(Options{Workdir: t.TempDir(), Link: link})
	if err != nil {
		t.Fatal(err)
	}
	url := first.URL()
	first.Close()
	second := start(t, Options{Workdir: t.TempDir(), Link: link})
	if second.URL() != url {
		t.Errorf("restarted at %s, want %s", second.URL(), url)
	}
	port := second.ln.Addr().(*net.TCPAddr).Port
	if _, err := Start(Options{Port: port}); err == nil {
		t.Error("Start on a taken --port should fail")
	}
	// The saved port is taken: a new one, with the same token.
	third := start(t, Options{Workdir: t.TempDir(), Link: link})
	if third.URL() == url || !strings.HasSuffix(third.URL(), url[strings.LastIndex(url[:len(url)-1], "/"):]) {
		t.Errorf("with the saved port taken, url = %s, want a new port and the token of %s", third.URL(), url)
	}
	writeFile(t, link, "not json")
	if fresh := start(t, Options{Link: link}); fresh.URL() == third.URL() {
		t.Error("an unreadable link file should give a new token")
	}
}

// TestPagesNeverRenderJobDataAsHTML: a log line holding a script tag comes
// back from the API as JSON text, and the pages only ever set text: they
// never use innerHTML or a sink like it.
func TestPagesNeverRenderJobDataAsHTML(t *testing.T) {
	const evil = `<script>alert("pwned")</script><img src=x onerror=alert(1)>`
	workdir := t.TempDir()
	dir := writeJob(t, workdir, state.Job{Repo: "app", Job: "x", State: state.Done, Activity: evil, Roles: []string{"implement"}})
	writeFile(t, filepath.Join(dir, "claude.log"), "ok\n"+evil+"\n")
	writeFile(t, filepath.Join(dir, "checks", "step-1", "round-1.log"), evil+"\n")
	srv := start(t, Options{Workdir: workdir})

	for _, route := range []string{"api/jobs/app/x/log", "api/jobs/app/x", "api/jobs"} {
		code, body, kind := get(t, srv.URL()+route)
		if code != http.StatusOK || kind != "application/json" || strings.Contains(body, "<script>") {
			t.Errorf("GET %s: %d %s, raw markup in %s", route, code, kind, body)
		}
	}
	var tail LogTail
	getJSON(t, srv.URL()+"api/jobs/app/x/log", &tail)
	if len(tail.Lines) != 2 || tail.Lines[1] != evil {
		t.Errorf("log lines = %q, want the script line as text", tail.Lines)
	}
	res, err := http.Get(srv.URL() + "api/jobs/app/x/checks/1/1")
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if !strings.HasPrefix(res.Header.Get("Content-Type"), "text/plain") || res.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("a check log is served as %q, nosniff %q: a browser could run it as HTML", res.Header.Get("Content-Type"), res.Header.Get("X-Content-Type-Options"))
	}
	for name, page := range map[string][]byte{"overview.html": overviewPage, "job.html": jobPage} {
		for _, sink := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval("} {
			if strings.Contains(string(page), sink) {
				t.Errorf("%s uses %s: render job data with textContent", name, sink)
			}
		}
		if !strings.Contains(string(page), "n.textContent = v") || !strings.Contains(string(page), "createTextNode") {
			t.Errorf("%s doesn't build its nodes from text", name)
		}
	}
}
