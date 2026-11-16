package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// PR states for pr_create and --pr-create.
const (
	prDraft = "draft"
	prReady = "ready"
	prNone  = "none"
)

// ship pushes the branch and opens its PR, when asked to, once every step
// has passed. It runs as prf, outside the sandbox, with your own git and
// gh credentials; Claude itself is denied git push and gh. It never forces
// a push. It returns 0, or exitShip if pushing or opening the PR failed.
func (j *job) ship() int {
	if !j.push {
		return 0
	}
	j.setState(func(s *jobState) { s.Stage, s.Activity = "pushing", "" })
	logf("pushing %s", j.branch)
	out, err := run(j.dir, "git", "push", "-u", "origin", j.branch)
	j.event("PRF_PUSH branch=%s exit=%d", j.branch, exitCodeOf(err))
	if err != nil {
		logf("push failed, so no PR either: %s", oneLine(out, 300))
		return exitShip
	}
	j.setState(func(s *jobState) { s.Pushed = true })
	if j.prCreate == "" {
		return 0
	}

	j.setState(func(s *jobState) { s.Stage = "opening PR" })
	url, err := j.openPR()
	if err != nil {
		logf("pushed, but no PR: %v", err)
		j.event("PRF_PR branch=%s exit=1", j.branch)
		return exitShip
	}
	j.event("PRF_PR branch=%s url=%s", j.branch, url)
	j.setState(func(s *jobState) { s.PRURL = url })
	logf("PR: %s", url)
	return 0
}

// openPR opens a PR for the branch with gh, or finds the one already open
// (as after a resume), and returns its URL. The title and body come from the
// repo's pr_title_file and pr_body_file, relative to the checkout; without
// them, gh fills both in from the commits.
func (j *job) openPR() (string, error) {
	// Open ones only: gh pr view also finds a merged or closed PR, and a branch
	// name used again after a merge would then get no PR at all.
	if url, err := run(j.dir, "gh", "pr", "list", "--head", j.branch, "--state", "open", "--json", "url", "--jq", ".[0].url"); err == nil && strings.HasPrefix(url, "http") {
		logf("a PR for %s is already open", j.branch)
		return strings.TrimSpace(url), nil
	}

	args := []string{"pr", "create", "--head", j.branch}
	if j.base != "" {
		args = append(args, "--base", j.base)
	}
	if j.prCreate == prDraft {
		args = append(args, "--draft")
	}
	if j.prTitleFile == "" && j.prBodyFile == "" {
		args = append(args, "--fill")
	} else {
		title, err := os.ReadFile(filepath.Join(j.dir, j.prTitleFile))
		if j.prTitleFile == "" || err != nil {
			return "", fmt.Errorf("pr_title_file %q isn't in the checkout", j.prTitleFile)
		}
		body := filepath.Join(j.dir, j.prBodyFile)
		if _, err := os.Stat(body); j.prBodyFile == "" || err != nil {
			return "", fmt.Errorf("pr_body_file %q isn't in the checkout", j.prBodyFile)
		}
		args = append(args, "--title", strings.TrimSpace(string(title)), "--body-file", body)
	}
	out, err := run(j.dir, "gh", args...)
	if err != nil {
		return "", fmt.Errorf("gh pr create: %s", oneLine(out, 300))
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return strings.TrimSpace(lines[len(lines)-1]), nil // gh prints the URL last
}

// run runs a command in dir with prf's own environment and returns its
// combined output.
func run(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// exitCodeOf returns a finished command's exit code: 0 for no error, and 1
// when it didn't run at all.
func exitCodeOf(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		return 1
	}
	return 0
}
