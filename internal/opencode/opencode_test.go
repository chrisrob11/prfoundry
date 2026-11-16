package opencode

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/chrisrob11/prfoundry/internal/agent"
)

// config reads the permission rules a command passes in its environment.
func config(t *testing.T, cmd agent.Command) []rule {
	t.Helper()
	for _, kv := range cmd.Env {
		if v, ok := strings.CutPrefix(kv, "OPENCODE_CONFIG_CONTENT="); ok {
			var c struct {
				Permissions []rule `json:"permissions"`
			}
			if err := json.Unmarshal([]byte(v), &c); err != nil {
				t.Fatal(err)
			}
			return c.Permissions
		}
	}
	t.Fatalf("no OPENCODE_CONFIG_CONTENT in %q", cmd.Env)
	return nil
}

// instructions are the files a command's config hands opencode as
// instructions.
func instructions(t *testing.T, cmd agent.Command) []string {
	t.Helper()
	for _, kv := range cmd.Env {
		if v, ok := strings.CutPrefix(kv, "OPENCODE_CONFIG_CONTENT="); ok {
			var c struct {
				Instructions []string `json:"instructions"`
			}
			if err := json.Unmarshal([]byte(v), &c); err != nil {
				t.Fatal(err)
			}
			return c.Instructions
		}
	}
	return nil
}

// effect is what the last rule matching action and resource exactly says.
func effect(rules []rule, action, resource string) string {
	e := "ask"
	for _, r := range rules {
		if r.Action == action && (r.Resource == resource || r.Resource == "*") {
			e = r.Effect
		}
	}
	return e
}

func TestCommand(t *testing.T) {
	c := agent.Call{Prompt: "do it", Mode: "acceptEdits", Model: "anthropic/claude-sonnet-5-5", Dirs: []string{"/job/tmp"}, Extra: []string{"--agent", "build"}}
	cmd, err := New("opencode").Command(c, "/job/repo")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"run", "--standalone", "--format", "json", "--model", "anthropic/claude-sonnet-5-5", "--agent", "build", "do it"}
	if !slices.Equal(cmd.Args, want) {
		t.Errorf("args = %q, want %q", cmd.Args, want)
	}
	if !slices.Contains(cmd.Env, "PWD=/job/repo") {
		t.Errorf("env %q doesn't point PWD at the checkout", cmd.Env)
	}
	for _, kv := range []string{"OPENCODE_CONFIG_PROJECT_DISABLE=1", "OPENCODE_DISABLE_PROJECT_CONFIG=1"} {
		if !slices.Contains(cmd.Env, kv) {
			t.Errorf("env %q lets opencode load the repo's own config: no %s", cmd.Env, kv)
		}
	}
	if got := instructions(t, cmd); !slices.Equal(got, []string{"/job/repo/AGENTS.md"}) {
		t.Errorf("instructions = %q, want the checkout's AGENTS.md", got)
	}
	rules := config(t, cmd)
	for _, check := range []struct{ action, resource, want string }{
		{"edit", "x.go", "allow"},
		{"read", "x.go", "allow"},
		{"shell", "go test ./...", "ask"}, // a headless run rejects it
		{"shell", "gh *", "deny"},
		{"shell", "git push *", "deny"},
		{"edit", "*opencode.json", "deny"},
		{"external_directory", "/job/tmp/*", "allow"},
	} {
		if got := effect(rules, check.action, check.resource); got != check.want {
			t.Errorf("%s %s: %s, want %s", check.action, check.resource, got, check.want)
		}
	}
}

func TestCommandModes(t *testing.T) {
	for _, c := range []struct {
		mode       string
		auto       bool
		edit, bash string
	}{
		{"default", false, "ask", "ask"},
		{"dontAsk", false, "ask", "ask"},
		{"plan", false, "deny", "deny"},
		{"bypassPermissions", true, "ask", "ask"}, // --auto approves what asks
	} {
		cmd, err := New("opencode").Command(agent.Call{Prompt: "p", Mode: c.mode}, "/r")
		if err != nil {
			t.Fatalf("%s: %v", c.mode, err)
		}
		rules := config(t, cmd)
		if slices.Contains(cmd.Args, "--auto") != c.auto || effect(rules, "edit", "x.go") != c.edit || effect(rules, "shell", "make") != c.bash {
			t.Errorf("%s: args %q, rules %v", c.mode, cmd.Args, rules)
		}
		if effect(rules, "shell", "gh *") != "deny" {
			t.Errorf("%s: gh isn't denied", c.mode)
		}
	}
}

func TestCommandRefuses(t *testing.T) {
	for _, c := range []struct {
		call agent.Call
		want string
	}{
		{agent.Call{Mode: "acceptEdits", Settings: "/home/claude-settings.json"}, "--sandbox=false"},
		{agent.Call{Mode: "auto"}, `permission mode "auto"`},
		{agent.Call{Mode: "acceptEdits", Model: "opus"}, "provider/model"},
	} {
		if _, err := New("opencode").Command(c.call, "/r"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: got %v, want an error about %s", c.call, err, c.want)
		}
	}
}

func TestValidModel(t *testing.T) {
	for name, want := range map[string]bool{
		"anthropic/claude-sonnet-5-5": true,
		"ollama/gemma4:31b":           true,
		"openrouter/anthropic/x#high": true,
		"opus":                        false,
		"/model":                      false,
		"anthropic/":                  false,
		"a b/c":                       false,
	} {
		if ValidModel(name) != want {
			t.Errorf("ValidModel(%q) = %t", name, !want)
		}
	}
}
