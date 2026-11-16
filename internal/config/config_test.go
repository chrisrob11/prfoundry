package config

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/chrisrob11/prfoundry/internal/jobenv"
	"github.com/chrisrob11/prfoundry/internal/textfile"
)

func writeFileT(t *testing.T, path, s string) {
	t.Helper()
	if err := textfile.Write(path, s); err != nil {
		t.Fatal(err)
	}
}

func TestReadConfig(t *testing.T) {
	t.Setenv("HOME", "/home/me")
	path := filepath.Join(t.TempDir(), FileName)
	writeFileT(t, path, `{
  "workdir": "~/work",
  "pr_dir": "/plans",
  "claude_args": ["--model", "sonnet"],
  "repos": {
    "app": "git@github.com:me/app.git",
    "api": {"url": "git@github.com:me/api.git", "path": "~/code/api",
      "job_env": {"GOCACHE": "{{job}}/cache/go-build"}, "job_seed": [{"from": "~/Library/Caches/go-build", "to": "{{job}}/cache/go-build"}]}
  }
}`)

	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Workdir:    "/home/me/work",
		PRDir:      "/plans",
		ClaudeArgs: []string{"--model", "sonnet"},
		Repos: map[string]Repo{
			"app": {URL: "git@github.com:me/app.git"},
			"api": {URL: "git@github.com:me/api.git", Path: "/home/me/code/api",
				JobEnv:  map[string]string{"GOCACHE": "{{job}}/cache/go-build"},
				JobSeed: []jobenv.Seed{{From: "~/Library/Caches/go-build", To: "{{job}}/cache/go-build"}}},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("config =\n%+v\nwant\n%+v", got, want)
	}
}

func TestReadConfigMissingFile(t *testing.T) {
	got, err := Read(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || !reflect.DeepEqual(got, Config{}) {
		t.Errorf("got %+v, %v; want empty config and no error", got, err)
	}
}

func TestReadConfigErrors(t *testing.T) {
	tests := []struct {
		name, body, wantErr string
	}{
		{"unknown key", `{"workdri": "x"}`, `unknown field "workdri"`},
		{"unknown repo key", `{"repos": {"app": {"url": "x", "paht": "y"}}}`, `unknown field "paht"`},
		{"repo wrong type", `{"repos": {"app": 5}}`, "must be a URL string or an object"},
		{"bad syntax", `{"workdir": }`, FileName},
		{"disk percent too high", `{"max_disk_percent": 101}`, "max_disk_percent must be between 0 (off) and 100, not 101"},
		{"job_env bad name", `{"repos": {"app": {"job_env": {"GO CACHE": "x"}}}}`, `repo app: job_env: "GO CACHE" is not an environment variable name`},
		{"job_seed outside the job", `{"repos": {"app": {"job_seed": [{"from": "/a", "to": "{{job}}/../b"}]}}}`, `job_seed: to "{{job}}/../b" must be under {{job}}`},
		{"job_seed not under the job", `{"repos": {"app": {"job_seed": [{"from": "/a", "to": "/b"}]}}}`, `must be under {{job}}`},
		{"job_seed is the job", `{"repos": {"app": {"job_seed": [{"from": "/a", "to": "{{job}}"}]}}}`, `must be under {{job}}`},
		{"job_seed no from", `{"repos": {"app": {"job_seed": [{"to": "{{job}}/b"}]}}}`, `job_seed: "{{job}}/b" has no from`},
		{"job_seed unknown key", `{"repos": {"app": {"job_seed": [{"form": "/a", "to": "{{job}}/b"}]}}}`, `unknown field "form"`},
		{"disk percent negative", `{"max_disk_percent": -1}`, "max_disk_percent must be between 0 (off) and 100, not -1"},
		{"bad pr_create", `{"repos": {"app": {"pr_create": "maybe"}}}`, `pr_create must be "draft" or "ready"`},
		{"bad model", `{"model": "bad model"}`, `invalid model "bad model"`},
		{"bad agent", `{"agent": "codex"}`, `agent must be "claude" or "opencode", not "codex"`},
		{"bad repo agent", `{"repos": {"app": {"agent": "Claude"}}}`, `repo app: agent must be`},
		{"bad opencode_model", `{"opencode_model": "sonnet"}`, `opencode_model must be provider/model, not "sonnet"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), FileName)
			writeFileT(t, path, tt.body)
			_, err := Read(path)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestConfiguredRepos(t *testing.T) {
	home := t.TempDir()
	writeFileT(t, filepath.Join(home, "app", "pipeline.json"), "{}")
	writeFileT(t, filepath.Join(home, "api", "roles", "x.md"), "x")
	writeFileT(t, filepath.Join(home, "empty", "notes.txt"), "")
	writeFileT(t, filepath.Join(home, "roles", "review.md"), "shared")
	writeFileT(t, filepath.Join(home, FileName), "{}")
	if got, want := ConfiguredRepos(home), []string{"api", "app"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ConfiguredRepos = %v, want %v", got, want)
	}
}

func TestExpandHome(t *testing.T) {
	t.Setenv("HOME", "/home/me")
	for in, want := range map[string]string{
		"~":       "/home/me",
		"~/a/b":   "/home/me/a/b",
		"/abs":    "/abs",
		"rel":     "rel",
		"~other/": "~other/",
	} {
		if got := ExpandHome(in); got != want {
			t.Errorf("ExpandHome(%q) = %q, want %q", in, got, want)
		}
	}
}
