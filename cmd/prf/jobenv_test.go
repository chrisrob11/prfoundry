package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

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
			if err := writeJobEnv(job, "app", tt.env); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(filepath.Join(job, jobEnvFile))
			if tt.want == "" {
				if err == nil {
					t.Errorf("wrote %s with no job_env: %q", jobEnvFile, b)
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
		name string
		repo repoConfig
		want []string
	}{
		{name: "none"},
		{
			name: "paths under the job folder, cleaned, deduplicated, sorted",
			repo: repoConfig{
				JobEnv:  map[string]string{"GOCACHE": "{{job}}/cache/go-build/", "LINT": "{{job}}/cache/lint", "SAME": "{{job}}/cache/lint"},
				JobSeed: []jobSeed{{From: "/shared", To: "{{job}}/cache/go-build"}, {From: "/m", To: "{{job}}/mod"}},
			},
			want: []string{"/w/j/cache/go-build", "/w/j/cache/lint", "/w/j/mod"},
		},
		{
			name: "not the job folder itself, nor outside it, nor a non-path",
			repo: repoConfig{JobEnv: map[string]string{
				"A": "{{job}}", "B": "{{job}}/../other", "C": "/elsewhere", "D": "-x {{job}}/y", "E": "on", "F": "{{job}}x/y",
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := jobWriteDirs("/w/j", tt.repo); !slices.Equal(got, tt.want) {
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
			errOut := captureStderr(t)
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

			seedJob(job, []jobSeed{
				{From: filepath.Join(src, "go"), To: "{{job}}/cache/go"},
				{From: filepath.Join(src, "one.txt"), To: "{{job}}/one.txt"},
			})
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
			if !strings.Contains(errOut.String(), tt.wantLog) {
				t.Errorf("stderr = %q, want %q", errOut, tt.wantLog)
			}
		})
	}
}

// A seed that can't be made leaves nothing half-copied, and isn't fatal.
func TestSeedJobFailures(t *testing.T) {
	errOut := captureStderr(t)
	fakeCP(t, true)
	src, job := t.TempDir(), t.TempDir()
	writeFileT(t, filepath.Join(job, "blocked"), "a file")
	seedJob(job, []jobSeed{{From: src, To: "{{job}}/blocked/x"}, {From: src, To: "{{job}}/cache"}})
	if _, err := os.Stat(filepath.Join(job, "cache")); err == nil {
		t.Error("a failed copy was left behind")
	}
	for _, want := range []string{"warning: job_seed: mkdir", "warning: job_seed: copying " + src, "no space left"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr missing %q:\n%s", want, errOut)
		}
	}
}

// run writes <job>/mise.toml and seeds the job folder before the first step,
// and lets every step write to the folders job_env and job_seed name.
func TestRunJobEnv(t *testing.T) {
	shared := t.TempDir()
	writeFileT(t, filepath.Join(shared, "go-build", "00", "entry"), "warm")
	e := newEnv(t, `"repos": {
		"app": {"url": "{{origin}}", "job_env": {"GOCACHE": "{{job}}/cache/go-build", "GOLANGCI_LINT_CACHE": "{{job}}/cache/golangci"},
			"job_seed": [{"from": "`+filepath.Join(shared, "go-build")+`", "to": "{{job}}/cache/go-build"}, {"from": "/no/such/dir", "to": "{{job}}/cache/gone"}]},
		"plain": "{{origin}}"
	}`)
	errOut := captureStderr(t)

	if code := e.run(t, "run", "--repo", "app", "--branch", "b", "--role", "review"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	job := filepath.Join(e.root, "work", "app", "b")
	want := "[env]\nGOCACHE = \"" + job + "/cache/go-build\"\nGOLANGCI_LINT_CACHE = \"" + job + "/cache/golangci\"\n"
	if got := readFile(t, filepath.Join(job, jobEnvFile)); !strings.HasSuffix(got, want) {
		t.Errorf("mise.toml =\n%s\nwant it to end with\n%s", got, want)
	}
	if got := readFile(t, filepath.Join(job, "cache", "go-build", "00", "entry")); got != "warm" {
		t.Errorf("seeded cache entry = %q, want warm", got)
	}
	if !strings.Contains(errOut.String(), "warning: job_seed: not copying /no/such/dir") {
		t.Errorf("no warning for the missing seed:\n%s", errOut)
	}
	args := e.calls(t)[0][1]
	for _, d := range []string{"cache/gone", "cache/go-build", "cache/golangci"} {
		dir := filepath.Join(job, d)
		if !strings.Contains(args, "--add-dir "+dir+" ") {
			t.Errorf("claude not given --add-dir %s: %s", dir, args)
		}
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			t.Errorf("%s not made: %v", dir, err)
		}
	}

	// Resuming keeps them writable.
	os.Remove(e.log)
	if code := e.run(t, "resume", "app/b", "--from", "1"); code != 0 {
		t.Fatalf("resume: exit %d", code)
	}
	if args := e.calls(t)[0][1]; !strings.Contains(args, "--add-dir "+filepath.Join(job, "cache", "golangci")+" ") {
		t.Errorf("resume lost the job's write dirs: %s", args)
	}

	// Without job_env and job_seed, nothing changes.
	os.Remove(e.log)
	if code := e.run(t, "run", "--repo", "plain", "--branch", "b", "--role", "review"); code != 0 {
		t.Fatalf("plain: exit %d", code)
	}
	plain := filepath.Join(e.root, "work", "plain", "b")
	if _, err := os.Stat(filepath.Join(plain, jobEnvFile)); err == nil {
		t.Error("wrote a mise.toml with no job_env")
	}
	if args := e.calls(t)[0][1]; strings.Count(args, "--add-dir ") != 2 { // tmp and context
		t.Errorf("plain run got extra --add-dir: %s", args)
	}

	// apply in your own checkout writes no mise.toml and adds no write dirs,
	// even for a repo with job_env.
	checkout := filepath.Join(e.root, "checkout")
	mustGit(t, "", "clone", "-q", e.origin, checkout)
	os.Remove(e.log)
	if code := e.run(t, "apply", "--repo", "app", "--dir", checkout, "--prompt", "x"); code != 0 {
		t.Fatalf("apply: exit %d", code)
	}
	if _, err := os.Stat(filepath.Join(e.root, jobEnvFile)); err == nil {
		t.Error("apply wrote a mise.toml beside your checkout")
	}
	if args := e.calls(t)[0][1]; strings.Contains(args, "cache") {
		t.Errorf("apply in your checkout got the job's write dirs: %s", args)
	}
}

// The @check gate's srt sandbox may write where Claude's may.
func TestCheckSandboxJobWriteDirs(t *testing.T) {
	withFakeSRT(t)
	dir := t.TempDir()
	settings := filepath.Join(dir, settingsFile)
	writeFileT(t, settings, `{"sandbox": {"enabled": true, "filesystem": {"allowWrite": ["/shared/go-build"]}}}`)
	j := job{jobDir: dir, dir: filepath.Join(dir, "repo"), tmp: filepath.Join(dir, "tmp"), settings: settings,
		writeDirs: []string{filepath.Join(dir, "cache", "a"), filepath.Join(dir, "cache", "b")}}

	path, err := j.checkSandbox("/check-tmp")
	if err != nil {
		t.Fatal(err)
	}
	var s srtSettings
	if err := json.Unmarshal([]byte(readFile(t, path)), &s); err != nil {
		t.Fatal(err)
	}
	want := []string{j.dir, j.tmp, "/check-tmp", j.writeDirs[0], j.writeDirs[1], "/shared/go-build"}
	if !slices.Equal(s.Filesystem.AllowWrite, want) {
		t.Errorf("allowWrite = %v, want %v", s.Filesystem.AllowWrite, want)
	}
}
