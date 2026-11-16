package jobenv

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/chrisrob11/prfoundry/internal/textfile"
)

func writeFileT(t *testing.T, path, s string) {
	t.Helper()
	if err := textfile.Write(path, s); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, _ := os.ReadFile(path)
	return string(b)
}

// logBuffer collects what a logf was given, one line each.
type logBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuffer) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(&l.b, format+"\n", args...)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestWriteJobEnv(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string // "" means no file
	}{
		{name: "empty writes nothing"},
		{
			name: "{{job}} is the job folder, names sorted",
			env:  map[string]string{"GOCACHE": "{{job}}/cache/go-build", "A_FLAG": "on", "LINT": "{{job}}/cache/lint:{{job}}/x"},
			want: "[env]\nA_FLAG = \"on\"\nGOCACHE = \"JOB/cache/go-build\"\nLINT = \"JOB/cache/lint:JOB/x\"\n",
		},
		{
			name: "quotes are escaped, mise's own templates pass through",
			env:  map[string]string{"X": `say "hi" <&> \ {{config_root}}`},
			want: "[env]\nX = \"say \\\"hi\\\" <&> \\\\ {{config_root}}\"\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := t.TempDir()
			if err := Write(job, "app", tt.env); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(filepath.Join(job, File))
			if tt.want == "" {
				if err == nil {
					t.Errorf("wrote %s with no job_env: %q", File, b)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			header, body, _ := strings.Cut(string(b), "\n")
			if !strings.HasPrefix(header, "# Written by prf") || !strings.Contains(header, "app") {
				t.Errorf("header = %q", header)
			}
			if want := strings.ReplaceAll(tt.want, "JOB", job); body != want {
				t.Errorf("mise.toml =\n%s\nwant\n%s", body, want)
			}
		})
	}
}

func TestJobWriteDirs(t *testing.T) {
	tests := []struct {
		name  string
		env   map[string]string
		seeds []Seed
		want  []string
	}{
		{name: "none"},
		{
			name:  "paths under the job folder, cleaned, deduplicated, sorted",
			env:   map[string]string{"GOCACHE": "{{job}}/cache/go-build/", "LINT": "{{job}}/cache/lint", "SAME": "{{job}}/cache/lint"},
			seeds: []Seed{{From: "/shared", To: "{{job}}/cache/go-build"}, {From: "/m", To: "{{job}}/mod"}},
			want:  []string{"/w/j/cache/go-build", "/w/j/cache/lint", "/w/j/mod"},
		},
		{
			name: "not the job folder itself, nor outside it, nor a non-path",
			env: map[string]string{
				"A": "{{job}}", "B": "{{job}}/../other", "C": "/elsewhere", "D": "-x {{job}}/y", "E": "on", "F": "{{job}}x/y",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := WriteDirs("/w/j", tt.env, tt.seeds); !slices.Equal(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// fakeCP puts a cp on PATH that refuses -c, as GNU cp does, and otherwise
// runs the real one, or with all set refuses everything.
func fakeCP(t *testing.T, all bool) {
	t.Helper()
	bin := t.TempDir()
	script := "#!/bin/sh\n[ \"$1\" = -c ] && { echo 'cp: invalid option -- c' >&2; exit 1; }\nexec /bin/cp \"$@\"\n"
	if all {
		script = "#!/bin/sh\nmkdir -p \"$3\"; echo 'cp: no space left' >&2; exit 1\n"
	}
	writeFileT(t, filepath.Join(bin, "cp"), script)
	if err := os.Chmod(filepath.Join(bin, "cp"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestSeedJob(t *testing.T) {
	tests := []struct {
		name      string
		noClone   bool // cp -c fails, as on Linux
		existing  bool // the target is already there
		missing   bool // the source isn't
		wantFiles map[string]string
		wantLog   string
	}{
		{name: "folder and file copied", wantFiles: map[string]string{"cache/go/a/b.txt": "b", "cache/go/c.txt": "c", "one.txt": "one"}},
		{name: "plain copy where cp -c fails", noClone: true, wantFiles: map[string]string{"cache/go/a/b.txt": "b", "one.txt": "one"}},
		{name: "missing source is a warning", missing: true, wantFiles: map[string]string{"one.txt": "one"}, wantLog: "warning: job_seed: not copying"},
		{name: "existing target is left alone", existing: true, wantFiles: map[string]string{"cache/go/mine.txt": "mine", "one.txt": "one"}, wantLog: "already exists"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var log logBuffer
			if tt.noClone {
				fakeCP(t, false)
			}
			src, job := t.TempDir(), t.TempDir()
			if !tt.missing {
				writeFileT(t, filepath.Join(src, "go", "a", "b.txt"), "b")
				writeFileT(t, filepath.Join(src, "go", "c.txt"), "c")
			}
			writeFileT(t, filepath.Join(src, "one.txt"), "one")
			if tt.existing {
				writeFileT(t, filepath.Join(job, "cache", "go", "mine.txt"), "mine")
			}

			Copy(job, []Seed{
				{From: filepath.Join(src, "go"), To: "{{job}}/cache/go"},
				{From: filepath.Join(src, "one.txt"), To: "{{job}}/one.txt"},
			}, log.logf)
			for rel, want := range tt.wantFiles {
				if got := readFile(t, filepath.Join(job, rel)); got != want {
					t.Errorf("%s = %q, want %q", rel, got, want)
				}
			}
			if tt.existing || tt.missing {
				if _, err := os.Stat(filepath.Join(job, "cache", "go", "c.txt")); err == nil {
					t.Error("copied into a target that should have been skipped")
				}
			}
			if !strings.Contains(log.String(), tt.wantLog) {
				t.Errorf("log = %q, want %q", log.String(), tt.wantLog)
			}
		})
	}
}

// A seed that can't be made leaves nothing half-copied, and isn't fatal.
func TestSeedJobFailures(t *testing.T) {
	var log logBuffer
	fakeCP(t, true)
	src, job := t.TempDir(), t.TempDir()
	writeFileT(t, filepath.Join(job, "blocked"), "a file")
	Copy(job, []Seed{{From: src, To: "{{job}}/blocked/x"}, {From: src, To: "{{job}}/cache"}}, log.logf)
	if _, err := os.Stat(filepath.Join(job, "cache")); err == nil {
		t.Error("a failed copy was left behind")
	}
	for _, want := range []string{"warning: job_seed: mkdir", "warning: job_seed: copying " + src, "no space left"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log missing %q:\n%s", want, log.String())
		}
	}
}
