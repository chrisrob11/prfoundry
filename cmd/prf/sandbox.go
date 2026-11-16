package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// settingsFile is the Claude Code settings file passed to every phase. It is
// looked up in home/<repo>/ and then home/, and the home one is created with
// sandbox defaults on first use.
const settingsFile = "claude-settings.json"

// claudeSettings is the subset of Claude Code's settings.json that the
// default file sets.
type claudeSettings struct {
	Sandbox     sandboxSettings     `json:"sandbox"`
	Permissions permissionsSettings `json:"permissions"`
}

type sandboxSettings struct {
	Enabled                      bool               `json:"enabled"`
	AutoAllowBashIfSandboxed     bool               `json:"autoAllowBashIfSandboxed"`
	FailIfUnavailable            bool               `json:"failIfUnavailable"`
	AllowUnsandboxedCommands     bool               `json:"allowUnsandboxedCommands"`
	EnableWeakerNetworkIsolation bool               `json:"enableWeakerNetworkIsolation"`
	Network                      networkSettings    `json:"network"`
	Filesystem                   filesystemSettings `json:"filesystem"`
}

type networkSettings struct {
	AllowedDomains      []string `json:"allowedDomains"`
	StrictAllowlist     bool     `json:"strictAllowlist"`
	AllowLocalBinding   bool     `json:"allowLocalBinding,omitempty"`
	AllowUnixSockets    []string `json:"allowUnixSockets,omitempty"`
	AllowAllUnixSockets bool     `json:"allowAllUnixSockets,omitempty"`
}

type filesystemSettings struct {
	AllowWrite []string `json:"allowWrite"`
	DenyWrite  []string `json:"denyWrite,omitempty"`
	DenyRead   []string `json:"denyRead"`
}

type permissionsSettings struct {
	Deny []string `json:"deny"`
}

// defaultSettings turns on Claude Code's sandbox: Bash runs without prompts
// but can only write to the working directory, the Go caches and mise's state, can only
// reach GitHub and the Go module proxy, and neither Bash nor the Read tool can
// read credentials. Weaker network isolation is needed on macOS for Go's TLS
// certificate checks to work inside the sandbox.
func defaultSettings() []byte {
	b, _ := json.MarshalIndent(claudeSettings{
		Sandbox: sandboxSettings{
			Enabled:                      true,
			AutoAllowBashIfSandboxed:     true,
			FailIfUnavailable:            true,
			AllowUnsandboxedCommands:     false,
			EnableWeakerNetworkIsolation: true,
			Network: networkSettings{
				AllowedDomains: []string{
					"github.com", "*.github.com", "*.githubusercontent.com",
					"proxy.golang.org", "sum.golang.org", "storage.googleapis.com",
				},
				StrictAllowlist: true,
			},
			Filesystem: filesystemSettings{
				AllowWrite: toolWriteDirs(),
				DenyRead:   []string{"~/.ssh", "~/.aws", "~/.gnupg", "~/.config/gh", "~/.netrc", "~/.docker"},
			},
		},
		// prf pushes and opens PRs itself (--push), outside the sandbox and
		// only after every step passes, so Claude never gets to.
		Permissions: permissionsSettings{Deny: []string{
			"Bash(git push:*)", "Bash(rtk git push:*)", "Bash(gh:*)", "Bash(rtk gh:*)",
			"Read(~/.ssh/**)", "Read(~/.aws/**)", "Read(~/.gnupg/**)",
			"Read(~/.config/gh/**)", "Read(~/.netrc)", "Read(~/.docker/**)",
		}},
	}, "", "  ")
	return append(b, '\n')
}

// settingsPath returns the settings file for repo, creating the shared
// default if neither it nor a repo override exists.
func settingsPath(repo string) (string, error) {
	repoLevel := filepath.Join(home, repo, settingsFile)
	if _, err := os.Stat(repoLevel); err == nil {
		return repoLevel, nil
	}
	shared := filepath.Join(home, settingsFile)
	_, err := os.Stat(shared)
	if err == nil {
		return shared, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if err := writeFile(shared, string(defaultSettings())); err != nil {
		return "", err
	}
	logf("wrote default sandbox settings to %s; edit it to change what Claude can reach", shared)
	return shared, nil
}

// toolWriteDirs are the directories outside the clone that Go and mise write to.
func toolWriteDirs() []string {
	dirs := goCacheDirs()
	if d := miseStateDir(); d != "" {
		dirs = append(dirs, d)
	}
	return dirs
}

// goCacheDirs returns the module cache, build cache, and checksum database
// cache, which Go writes to outside the working directory. It asks go, and
// falls back to Go's defaults when go can't run here (e.g. mise has no
// version set for this directory).
func goCacheDirs() []string {
	modcache, gocache, gopath := os.Getenv("GOMODCACHE"), os.Getenv("GOCACHE"), os.Getenv("GOPATH")
	out, err := exec.Command("go", "env", "GOMODCACHE", "GOCACHE", "GOPATH").Output()
	if dirs := strings.Split(strings.TrimSpace(string(out)), "\n"); err == nil && len(dirs) == 3 {
		modcache, gocache, gopath = dirs[0], dirs[1], dirs[2]
	}
	if gopath == "" {
		gopath = filepath.Join(homeDir(), "go")
	}
	gopath = filepath.SplitList(gopath)[0]
	if modcache == "" {
		modcache = filepath.Join(gopath, "pkg", "mod")
	}
	if gocache == "" {
		if dir, err := os.UserCacheDir(); err == nil {
			gocache = filepath.Join(dir, "go-build")
		}
	}
	return slices.DeleteFunc([]string{modcache, gocache, filepath.Join(gopath, "pkg", "sumdb")},
		func(s string) bool { return s == "" })
}
