package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"
)

func TestReadConfig(t *testing.T) {
	t.Setenv("HOME", "/home/me")
	path := filepath.Join(t.TempDir(), confFile)
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

	got, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	want := config{
		Workdir:    "/home/me/work",
		PRDir:      "/plans",
		ClaudeArgs: []string{"--model", "sonnet"},
		Repos: map[string]repoConfig{
			"app": {URL: "git@github.com:me/app.git"},
			"api": {URL: "git@github.com:me/api.git", Path: "/home/me/code/api",
				JobEnv:  map[string]string{"GOCACHE": "{{job}}/cache/go-build"},
				JobSeed: []jobSeed{{From: "~/Library/Caches/go-build", To: "{{job}}/cache/go-build"}}},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("config =\n%+v\nwant\n%+v", got, want)
	}
}

func TestReadConfigMissingFile(t *testing.T) {
	got, err := readConfig(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || !reflect.DeepEqual(got, config{}) {
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
		{"bad syntax", `{"workdir": }`, confFile},
		{"disk percent too high", `{"max_disk_percent": 101}`, "max_disk_percent must be between 0 (off) and 100, not 101"},
		{"job_env bad name", `{"repos": {"app": {"job_env": {"GO CACHE": "x"}}}}`, `repo app: job_env: "GO CACHE" is not an environment variable name`},
		{"job_seed outside the job", `{"repos": {"app": {"job_seed": [{"from": "/a", "to": "{{job}}/../b"}]}}}`, `job_seed: to "{{job}}/../b" must be under {{job}}`},
		{"job_seed not under the job", `{"repos": {"app": {"job_seed": [{"from": "/a", "to": "/b"}]}}}`, `must be under {{job}}`},
		{"job_seed is the job", `{"repos": {"app": {"job_seed": [{"from": "/a", "to": "{{job}}"}]}}}`, `must be under {{job}}`},
		{"job_seed no from", `{"repos": {"app": {"job_seed": [{"to": "{{job}}/b"}]}}}`, `job_seed: "{{job}}/b" has no from`},
		{"job_seed unknown key", `{"repos": {"app": {"job_seed": [{"form": "/a", "to": "{{job}}/b"}]}}}`, `unknown field "form"`},
		{"disk percent negative", `{"max_disk_percent": -1}`, "max_disk_percent must be between 0 (off) and 100, not -1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), confFile)
			writeFileT(t, path, tt.body)
			_, err := readConfig(path)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestConfigInit(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".prf")
	if code := runPrf(t, "--home", h, "config", "init"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, f := range []string{confFile, settingsFile, "roles/implement.md", "roles/test.md", "roles/review.md"} {
		if _, err := os.Stat(filepath.Join(h, f)); err != nil {
			t.Errorf("%s not written: %v", f, err)
		}
	}
	if _, err := readConfig(filepath.Join(h, confFile)); err != nil {
		t.Errorf("starter conf.json doesn't load: %v", err)
	}

	// A second init must not overwrite edits.
	review := filepath.Join(h, "roles", "review.md")
	writeFileT(t, review, "my review")
	if code := runPrf(t, "--home", h, "config", "init"); code != 0 {
		t.Fatalf("second init: exit %d", code)
	}
	if b, _ := os.ReadFile(review); string(b) != "my review" {
		t.Errorf("init overwrote an existing role: %q", b)
	}
}

func writeFileT(t *testing.T, path, s string) {
	t.Helper()
	if err := writeFile(path, s); err != nil {
		t.Fatal(err)
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
		if got := expandHome(in); got != want {
			t.Errorf("expandHome(%q) = %q, want %q", in, got, want)
		}
	}
}
func TestFlagOr(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		env        string
		fromConfig string
		want       string
	}{
		{name: "flag wins", args: []string{"--workdir", "flag"}, env: "env", fromConfig: "cfg", want: "flag"},
		{name: "env beats config", env: "env", fromConfig: "cfg", want: "env"},
		{name: "empty flag falls through", args: []string{"--workdir", ""}, fromConfig: "cfg", want: "cfg"},
		{name: "config beats default", fromConfig: "cfg", want: "cfg"},
		{name: "default", want: "default"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.env != "" {
				t.Setenv("TEST_WORKDIR", tt.env)
			}
			var got string
			cmd := &cli.Command{
				Flags: []cli.Flag{&cli.StringFlag{Name: "workdir", Value: "default", Sources: cli.EnvVars("TEST_WORKDIR")}},
				Action: func(_ context.Context, c *cli.Command) error {
					got = flagOr(c, "workdir", tt.fromConfig)
					return nil
				},
			}
			if err := cmd.Run(context.Background(), append([]string{"t"}, tt.args...)); err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
