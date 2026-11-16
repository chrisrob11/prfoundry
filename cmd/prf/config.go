package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/chrisrob11/prfoundry/internal/pipeline"
	"github.com/chrisrob11/prfoundry/internal/roles"
	"github.com/urfave/cli/v3"
)

// confFile is the global config, in the prf home dir.
const confFile = "conf.json"

// home is the prf home dir, normally ~/.prf. Set by loadConfig.
var home string

// cfg is home/conf.json, loaded by loadConfig. Flags and env vars override it.
var cfg config

type config struct {
	Workdir      string                `json:"workdir,omitempty"`
	PRDir        string                `json:"pr_dir,omitempty"`
	BranchPrefix string                `json:"branch_prefix,omitempty"`
	Mode         string                `json:"mode,omitempty"`
	Model        string                `json:"model,omitempty"`
	MaxCost      float64               `json:"max_cost,omitempty"`
	MaxStepCost  float64               `json:"max_step_cost,omitempty"`
	Claude       string                `json:"claude,omitempty"`
	ClaudeArgs   []string              `json:"claude_args,omitempty"`
	Sandbox      *bool                 `json:"sandbox,omitempty"`
	ReviewOpen   *bool                 `json:"review_open,omitempty"`      // open the page when a job reaches a @review or @ask gate; default true
	MaxDisk      int                   `json:"max_disk_percent,omitempty"` // run refuses to clone when the workdir's disk is this full; 0 is off
	Repos        map[string]repoConfig `json:"repos,omitempty"`
}

// repoConfig is a repo alias: --repo <alias> picks up these settings.
type repoConfig struct {
	URL         string            `json:"url,omitempty"`
	Path        string            `json:"path,omitempty"`
	Base        string            `json:"base,omitempty"`
	PRDir       string            `json:"pr_dir,omitempty"`
	Push        bool              `json:"push,omitempty"`          // push the branch once every step passes
	PRCreate    string            `json:"pr_create,omitempty"`     // after pushing, open a "draft" or "ready" PR
	PRTitleFile string            `json:"pr_title_file,omitempty"` // in the checkout; without it, gh --fill
	PRBodyFile  string            `json:"pr_body_file,omitempty"`
	JobEnv      map[string]string `json:"job_env,omitempty"`  // written to <job>/mise.toml's [env]; {{job}} is the job folder
	JobSeed     []jobSeed         `json:"job_seed,omitempty"` // copied into the job folder before the first step
}

// UnmarshalJSON accepts either a bare URL ("app": "git@...") or an object.
func (r *repoConfig) UnmarshalJSON(b []byte) error {
	var url string
	if err := json.Unmarshal(b, &url); err == nil {
		r.URL = url
		return nil
	}
	type plain repoConfig
	if err := decodeStrict(b, (*plain)(r)); err != nil {
		return fmt.Errorf("repo must be a URL string or an object with url, path, base, pr_dir: %w", err)
	}
	return nil
}

const confTemplate = `{
  "workdir": "~/prf-work",
  "pr_dir": "",
  "branch_prefix": "prf/",
  "claude_args": [],
  "repos": {}
}
`

func defaultHome() string {
	return filepath.Join(homeDir(), ".prf")
}

func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}

// decodeStrict decodes JSON, rejecting unknown fields so typos are caught.
func decodeStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// readConfig parses the config at path. A missing file gives an empty config.
func readConfig(path string) (config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return config{}, nil
	}
	var c config
	if err == nil {
		err = decodeStrict(b, &c)
	}
	if err != nil {
		return config{}, fmt.Errorf("%s: %w", path, err)
	}

	if c.MaxDisk < 0 || c.MaxDisk > 100 {
		return config{}, fmt.Errorf("%s: max_disk_percent must be between 0 (off) and 100, not %d", path, c.MaxDisk)
	}
	c.Workdir, c.PRDir = expandHome(c.Workdir), expandHome(c.PRDir)
	for name, r := range c.Repos {
		if r.PRCreate != "" && r.PRCreate != prDraft && r.PRCreate != prReady {
			return config{}, fmt.Errorf("%s: repo %s: pr_create must be %q or %q", path, name, prDraft, prReady)
		}
		if err := checkJobEnv(r); err != nil {
			return config{}, fmt.Errorf("%s: repo %s: %w", path, name, err)
		}
		r.Path, r.PRDir = expandHome(r.Path), expandHome(r.PRDir)
		c.Repos[name] = r
	}
	return c, nil
}

// loadConfig is the root Before hook: it sets home and cfg.
func loadConfig(ctx context.Context, c *cli.Command) (context.Context, error) {
	var err error
	if home, err = filepath.Abs(expandHome(c.String("home"))); err != nil {
		return ctx, err
	}
	if cfg, err = readConfig(filepath.Join(home, confFile)); err != nil {
		return ctx, err
	}
	if cfg.Model != "" && !pipeline.ValidModel(cfg.Model) {
		return ctx, fmt.Errorf("%s: invalid model %q", filepath.Join(home, confFile), cfg.Model)
	}
	return ctx, nil
}

func configCmd() *cli.Command {
	return &cli.Command{
		Name:  "config",
		Usage: "show the prf home, its conf.json, and the repos it has pipelines and roles for",
		Action: func(ctx context.Context, c *cli.Command) error {
			w := c.Root().Writer
			path := filepath.Join(home, confFile)
			if _, err := os.Stat(path); err != nil {
				_, _ = fmt.Fprintf(w, "# %s (not found; create it with: prf config init)\n", path)
			} else {
				_, _ = fmt.Fprintf(w, "# %s\n", path)
			}
			for _, repo := range configuredRepos() {
				_, _ = fmt.Fprintf(w, "# repo %s: %s\n", repo, filepath.Join(home, repo))
			}
			return writeJSON(w, cfg)
		},
		Commands: []*cli.Command{{
			Name:   "init",
			Usage:  "write a starter conf.json, the built-in roles, and the sandbox settings, skipping files that exist",
			Action: initConfig,
		}},
	}
}

// configuredRepos returns the dirs in home that hold a pipeline.json or roles/.
func configuredRepos() []string {
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

func initConfig(ctx context.Context, c *cli.Command) error {
	files := [][2]string{
		{filepath.Join(home, confFile), confTemplate},
		{filepath.Join(home, settingsFile), string(defaultSettings())},
	}
	entries, err := fs.ReadDir(roles.Builtin(), ".")
	if err != nil {
		return err
	}
	for _, e := range entries {
		b, err := fs.ReadFile(roles.Builtin(), e.Name())
		if err != nil {
			return err
		}
		files = append(files, [2]string{filepath.Join(home, "roles", e.Name()), string(b)})
	}

	w := c.Root().Writer
	for _, f := range files {
		if _, err := os.Stat(f[0]); err == nil {
			_, _ = fmt.Fprintln(w, "exists", f[0])
			continue
		}
		if err := writeFile(f[0], f[1]); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(w, "wrote ", f[0])
	}
	return nil
}

// flagOr returns a flag's value if it was given (non-empty) on the command
// line or via env, otherwise the first non-empty fallback, otherwise the
// flag's default.
func flagOr(c *cli.Command, name string, fallbacks ...string) string {
	if v := c.String(name); c.IsSet(name) && v != "" {
		return v
	}
	for _, f := range fallbacks {
		if f != "" {
			return f
		}
	}
	return c.String(name)
}

func expandHome(p string) string {
	if p == "~" {
		return homeDir()
	}
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		return filepath.Join(homeDir(), rest)
	}
	return p
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
