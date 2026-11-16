package opencode

import (
	"encoding/json"
	"regexp"
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

// builtIn opens the rules of every opencode agent: allow everything.
var builtIn = rule{"*", "*", "allow"}

// matches is opencode's wildcard match: * is any text, ? any character,
// and a trailing " *" also matches nothing, so "gh *" matches "gh".
func matches(s, pattern string) bool {
	re := strings.NewReplacer(`\*`, ".*", `\?`, ".").Replace(regexp.QuoteMeta(pattern))
	if r, ok := strings.CutSuffix(re, " .*"); ok {
		re = r + "( .*)?"
	}
	return regexp.MustCompile("(?s)^" + re + "$").MatchString(s)
}

// effect is what opencode makes of action on resource: the last of the
// built-in rule and rules that matches both, else ask.
func effect(rules []rule, action, resource string) string {
	e := "ask"
	for _, r := range append([]rule{builtIn}, rules...) {
		if matches(action, r.Action) && matches(resource, r.Resource) {
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
		{"shell", "gh pr create", "deny"},
		{"shell", "git push origin b", "deny"},
		{"edit", "/job/repo/opencode.json", "deny"},
		{"external_directory", "/job/tmp/x", "allow"},
		{"external_directory", "/elsewhere/x", "ask"},
	} {
		if got := effect(rules, check.action, check.resource); got != check.want {
			t.Errorf("%s %s: %s, want %s", check.action, check.resource, got, check.want)
		}
	}
}

// In every mode but bypassPermissions, what no rule of prf's allows asks,
// and a headless run rejects it, though opencode's built-in rule allows
// everything.
func TestCommandModes(t *testing.T) {
	for _, c := range []struct {
		mode             string
		auto             bool
		edit, bash, rest string // rest: an action no mode names
	}{
		{"default", false, "ask", "ask", "ask"},
		{"dontAsk", false, "ask", "ask", "ask"},
		{"acceptEdits", false, "allow", "ask", "ask"},
		{"plan", false, "deny", "deny", "ask"},
		{"bypassPermissions", true, "allow", "allow", "allow"},
	} {
		cmd, err := New("opencode").Command(agent.Call{Prompt: "p", Mode: c.mode}, "/r")
		if err != nil {
			t.Fatalf("%s: %v", c.mode, err)
		}
		rules := config(t, cmd)
		if slices.Contains(cmd.Args, "--auto") != c.auto {
			t.Errorf("%s: args %q", c.mode, cmd.Args)
		}
		if first := len(rules) > 0 && rules[0] == askFirst; first == c.auto {
			t.Errorf("%s: rules %v; want them to start with %v only when not --auto", c.mode, rules, askFirst)
		}
		for _, check := range []struct{ action, resource, want string }{
			{"read", "x.go", "allow"},
			{"grep", "func", "allow"},
			{"todowrite", "*", "allow"},
			{"edit", "x.go", c.edit},
			{"shell", "make", c.bash},
			{"webfetch", "https://example.com", c.rest},
			{"external_directory", "/elsewhere/x", c.rest},
			{"shell", "gh", "deny"},
			{"shell", "rtk gh pr create", "deny"},
			{"shell", "git push -f origin b", "deny"},
			{"edit", "/r/opencode.jsonc", "deny"},
			{"edit", "/r/.opencode/agent/a.md", "deny"},
		} {
			if got := effect(rules, check.action, check.resource); got != check.want {
				t.Errorf("%s: %s %s: %s, want %s", c.mode, check.action, check.resource, got, check.want)
			}
		}
	}
}

func TestMatches(t *testing.T) {
	for _, c := range []struct {
		s, pattern string
		want       bool
	}{
		{"gh", "gh *", true},
		{"gh pr list", "gh *", true},
		{"ghost", "gh *", false},
		{"a/b.md", "*.md", true},
		{"x.go", "?.go", true},
		{"x.go", "x.g", false},
	} {
		if matches(c.s, c.pattern) != c.want {
			t.Errorf("matches(%q, %q) = %t", c.s, c.pattern, !c.want)
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
