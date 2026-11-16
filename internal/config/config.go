// Package config reads conf.json, prf's global config in the prf home:
// defaults for every job, and the repo aliases --repo can name. It also
// finds the default home and expands ~ in the paths a config holds.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/chrisrob11/prfoundry/internal/jobenv"
	"github.com/chrisrob11/prfoundry/internal/jsonio"
	"github.com/chrisrob11/prfoundry/internal/pipeline"
)

// FileName is the global config, in the prf home dir.
const FileName = "conf.json"

// The values pr_create may take.
const (
	PRDraft = "draft"
	PRReady = "ready"
)

// Config is home/conf.json. Flags and env vars override it.
type Config struct {
	Workdir      string          `json:"workdir,omitempty"`
	PRDir        string          `json:"pr_dir,omitempty"`
	BranchPrefix string          `json:"branch_prefix,omitempty"`
	Mode         string          `json:"mode,omitempty"`
	Model        string          `json:"model,omitempty"`
	MaxCost      float64         `json:"max_cost,omitempty"`
	MaxStepCost  float64         `json:"max_step_cost,omitempty"`
	Claude       string          `json:"claude,omitempty"`
	ClaudeArgs   []string        `json:"claude_args,omitempty"`
	Sandbox      *bool           `json:"sandbox,omitempty"`
	ReviewOpen   *bool           `json:"review_open,omitempty"`      // open the page when a job reaches a @review or @ask gate; default true
	MaxDisk      int             `json:"max_disk_percent,omitempty"` // run refuses to clone when the workdir's disk is this full; 0 is off
	Repos        map[string]Repo `json:"repos,omitempty"`
}

// Repo is a repo alias: --repo <alias> picks up these settings.
type Repo struct {
	URL         string            `json:"url,omitempty"`
	Path        string            `json:"path,omitempty"`
	Base        string            `json:"base,omitempty"`
	PRDir       string            `json:"pr_dir,omitempty"`
	Push        bool              `json:"push,omitempty"`          // push the branch once every step passes
	PRCreate    string            `json:"pr_create,omitempty"`     // after pushing, open a "draft" or "ready" PR
	PRTitleFile string            `json:"pr_title_file,omitempty"` // in the checkout; without it, gh --fill
	PRBodyFile  string            `json:"pr_body_file,omitempty"`
	JobEnv      map[string]string `json:"job_env,omitempty"`  // written to <job>/mise.toml's [env]; {{job}} is the job folder
	JobSeed     []jobenv.Seed     `json:"job_seed,omitempty"` // copied into the job folder before the first step
}

// UnmarshalJSON accepts either a bare URL ("app": "git@...") or an object.
func (r *Repo) UnmarshalJSON(b []byte) error {
	var url string
	if err := json.Unmarshal(b, &url); err == nil {
		r.URL = url
		return nil
	}
	type plain Repo
	if err := jsonio.DecodeStrict(b, (*plain)(r)); err != nil {
		return fmt.Errorf("repo must be a URL string or an object with url, path, base, pr_dir: %w", err)
	}
	return nil
}

// Template is the starter conf.json that config init writes.
const Template = `{
  "workdir": "~/prf-work",
  "pr_dir": "",
  "branch_prefix": "prf/",
  "claude_args": [],
  "repos": {}
}
`

// DefaultHome is the prf home when neither --home nor PRF_HOME gives one.
func DefaultHome() string {
	return filepath.Join(homeDir(), ".prf")
}

func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}

// Read parses and checks the config at path, with ~ expanded in its paths.
// A missing file gives an empty config.
func Read(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Config{}, nil
	}
	var c Config
	if err == nil {
		err = jsonio.DecodeStrict(b, &c)
	}
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}

	if c.MaxDisk < 0 || c.MaxDisk > 100 {
		return Config{}, fmt.Errorf("%s: max_disk_percent must be between 0 (off) and 100, not %d", path, c.MaxDisk)
	}
	c.Workdir, c.PRDir = ExpandHome(c.Workdir), ExpandHome(c.PRDir)
	for name, r := range c.Repos {
		if r.PRCreate != "" && r.PRCreate != PRDraft && r.PRCreate != PRReady {
			return Config{}, fmt.Errorf("%s: repo %s: pr_create must be %q or %q", path, name, PRDraft, PRReady)
		}
		if err := jobenv.Check(r.JobEnv, r.JobSeed); err != nil {
			return Config{}, fmt.Errorf("%s: repo %s: %w", path, name, err)
		}
		r.Path, r.PRDir = ExpandHome(r.Path), ExpandHome(r.PRDir)
		c.Repos[name] = r
	}
	if c.Model != "" && !pipeline.ValidModel(c.Model) {
		return Config{}, fmt.Errorf("%s: invalid model %q", path, c.Model)
	}
	return c, nil
}

// ConfiguredRepos returns the dirs in home that hold a pipeline.json or roles/.
func ConfiguredRepos(home string) []string {
	entries, _ := os.ReadDir(home)
	var repos []string
	for _, e := range entries {
		if e.Name() == "roles" || !e.IsDir() {
			continue
		}
		for _, f := range []string{pipeline.FileName, "roles"} {
			if _, err := os.Stat(filepath.Join(home, e.Name(), f)); err == nil {
				repos = append(repos, e.Name())
				break
			}
		}
	}
	return repos
}

// ExpandHome replaces a leading ~ or ~/ in p with your home dir.
func ExpandHome(p string) string {
	if p == "~" {
		return homeDir()
	}
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		return filepath.Join(homeDir(), rest)
	}
	return p
}
