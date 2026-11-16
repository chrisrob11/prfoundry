package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chrisrob11/prfoundry/internal/dashboard"
	"github.com/chrisrob11/prfoundry/internal/job"
	"github.com/chrisrob11/prfoundry/internal/state"
)

// lockedBuffer is a Writer a test can read while a command writes to it.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// startUI runs prf ui with args until the test ends, returning the URL it prints.
func startUI(t *testing.T, e env, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	app, out := e.app(), &lockedBuffer{}
	app.Reader, app.Writer = strings.NewReader(""), out
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx, append([]string{"prf", "--home", e.home, "ui"}, args...)) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("prf ui: %v", err)
		}
	})
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if url := strings.TrimSpace(out.String()); strings.HasPrefix(url, "http://") {
			return url
		}
	}
	t.Fatalf("prf ui printed no URL: %q", out.String())
	return ""
}

func TestUICommand(t *testing.T) {
	e := newEnv(t, "")
	work := filepath.Join(e.root, "work")
	b, _ := json.Marshal(state.Job{Repo: "app", Job: "login", State: state.Done, Updated: time.Now()})
	writeFileT(t, filepath.Join(work, "app", "login", state.FileName), string(b))
	opened := make(chan string, 1)
	e.h.rt.OpenBrowser = func(url string) error { opened <- url; return nil }

	url := startUI(t, e, "--no-open")
	res, err := http.Get(url + "api/jobs")
	if err != nil {
		t.Fatal(err)
	}
	var o dashboard.Overview
	err = json.NewDecoder(res.Body).Decode(&o)
	_ = res.Body.Close()
	if err != nil || len(o.Jobs) != 1 || o.Jobs[0].Job != "login" {
		t.Errorf("overview = %+v, %v; want conf.json's workdir's job", o, err)
	}
	res, err = http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if !strings.Contains(string(page), "prf jobs") {
		t.Error("overview page not served")
	}
	select {
	case u := <-opened:
		t.Errorf("--no-open opened %s", u)
	default:
	}

	// Without --no-open, the browser opens the same URL: the token is kept
	// in the home. The first server still has its port, so this one picks another.
	second := startUI(t, e, "--repo", "app")
	select {
	case u := <-opened:
		if u != second || u[strings.LastIndex(u[:len(u)-1], "/"):] != url[strings.LastIndex(url[:len(url)-1], "/"):] {
			t.Errorf("opened %s, want %s with the token of %s", u, second, url)
		}
	case <-time.After(time.Second):
		t.Error("prf ui didn't open the browser")
	}

	port := strings.Split(strings.Split(url, "/")[2], ":")[1]
	if code, msg := e.runErr(t, "ui", "--no-open", "--port", port); code != job.ExitUsage || !strings.Contains(msg, "port "+port) {
		t.Errorf("ui on a taken port = %d %q", code, msg)
	}
}
