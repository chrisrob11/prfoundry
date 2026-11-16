package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// miseConfigs are the project files that tell mise which tool versions to use.
var miseConfigs = []string{
	"mise.toml", ".mise.toml", "mise.local.toml", ".tool-versions",
	".config/mise.toml", ".config/mise/config.toml", "mise/config.toml", ".mise/config.toml",
}

// miseEnv returns the environment for mise and Claude. When trusted is set,
// mise trusts config files under it, so fresh clones there work without
// `mise trust`; nothing outside it is trusted that wasn't already.
func miseEnv(trusted string) []string {
	env := os.Environ()
	if trusted == "" {
		return env
	}
	paths := trusted
	if prev := os.Getenv("MISE_TRUSTED_CONFIG_PATHS"); prev != "" {
		paths += string(os.PathListSeparator) + prev
	}
	return append(env, "MISE_TRUSTED_CONFIG_PATHS="+paths)
}

// trustedDir is what mise may trust for a job in dir: the workdir when dir
// is prf's own clone there, and nothing new for your own checkout.
func trustedDir(dir, workdir string) string {
	if strings.HasPrefix(dir, workdir+string(filepath.Separator)) {
		return workdir
	}
	return ""
}

// installTools runs `mise install` in dir if it has a mise config, before
// Claude starts: the sandbox blocks mise from downloading missing versions.
// A failure is only a warning, since Claude may not need every tool.
func installTools(dir string, env []string) {
	if _, err := exec.LookPath("mise"); err != nil || !hasMiseConfig(dir) {
		return
	}
	logf("installing tools with mise")
	cmd := exec.Command("mise", "install", "--yes")
	cmd.Dir, cmd.Env = dir, env
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		logf("warning: mise install failed: %v", err)
	}
}

func hasMiseConfig(dir string) bool {
	for _, name := range miseConfigs {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

// miseStateDir is where mise records which configs it has seen. It writes
// there on every run, so the sandbox must allow it.
func miseStateDir() string {
	if _, err := exec.LookPath("mise"); err != nil {
		return ""
	}
	if d := os.Getenv("MISE_STATE_DIR"); d != "" {
		return d
	}
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		state = filepath.Join(homeDir(), ".local", "state")
	}
	return filepath.Join(state, "mise")
}
