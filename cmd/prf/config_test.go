package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/chrisrob11/prfoundry/internal/config"
	"github.com/chrisrob11/prfoundry/internal/sandbox"
	"github.com/chrisrob11/prfoundry/internal/textfile"
	"github.com/urfave/cli/v3"
)

func TestConfigInit(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".prf")
	if code := runPrf(t, "--home", h, "config", "init"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, f := range []string{config.FileName, sandbox.SettingsFile, "roles/implement.md", "roles/test.md", "roles/review.md"} {
		if _, err := os.Stat(filepath.Join(h, f)); err != nil {
			t.Errorf("%s not written: %v", f, err)
		}
	}
	if _, err := config.Read(filepath.Join(h, config.FileName)); err != nil {
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
	if err := textfile.Write(path, s); err != nil {
		t.Fatal(err)
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
