package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/chrisrob11/prfoundry/internal/config"
	"github.com/chrisrob11/prfoundry/internal/jsonio"
	"github.com/chrisrob11/prfoundry/internal/roles"
	"github.com/chrisrob11/prfoundry/internal/sandbox"
	"github.com/chrisrob11/prfoundry/internal/textfile"
	"github.com/urfave/cli/v3"
)

// home is the prf home dir, normally ~/.prf. Set by loadConfig.
var home string

// cfg is home/conf.json, loaded by loadConfig. Flags and env vars override it.
var cfg config.Config

// loadConfig is the root Before hook: it sets home and cfg.
func loadConfig(ctx context.Context, c *cli.Command) (context.Context, error) {
	var err error
	if home, err = filepath.Abs(config.ExpandHome(c.String("home"))); err != nil {
		return ctx, err
	}
	if cfg, err = config.Read(filepath.Join(home, config.FileName)); err != nil {
		return ctx, err
	}
	return ctx, nil
}

func configCmd() *cli.Command {
	return &cli.Command{
		Name:  "config",
		Usage: "show the prf home, its conf.json, and the repos it has pipelines and roles for",
		Action: func(ctx context.Context, c *cli.Command) error {
			w := c.Root().Writer
			path := filepath.Join(home, config.FileName)
			if _, err := os.Stat(path); err != nil {
				_, _ = fmt.Fprintf(w, "# %s (not found; create it with: prf config init)\n", path)
			} else {
				_, _ = fmt.Fprintf(w, "# %s\n", path)
			}
			for _, repo := range config.ConfiguredRepos(home) {
				_, _ = fmt.Fprintf(w, "# repo %s: %s\n", repo, filepath.Join(home, repo))
			}
			return jsonio.Write(w, cfg)
		},
		Commands: []*cli.Command{{
			Name:   "init",
			Usage:  "write a starter conf.json, the built-in roles, and the sandbox settings, skipping files that exist",
			Action: initConfig,
		}},
	}
}

func initConfig(ctx context.Context, c *cli.Command) error {
	files := [][2]string{
		{filepath.Join(home, config.FileName), config.Template},
		{filepath.Join(home, sandbox.SettingsFile), string(sandbox.DefaultSettings())},
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
		if err := textfile.Write(f[0], f[1]); err != nil {
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

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
