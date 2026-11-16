// Package ship pushes a job's branch and opens its pull request. It runs
// git and gh as you, outside the sandbox, with your own credentials; Claude
// itself is denied git push and gh. It never forces a push.
package ship

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/chrisrob11/prfoundry/internal/human"
)

// Push pushes branch from the checkout dir to origin, setting its upstream,
// and returns git's output.
func Push(dir, branch string) (string, error) {
	return run(dir, "git", "push", "-u", "origin", branch)
}

// PR is the pull request to open for a pushed branch.
type PR struct {
	Dir    string // the checkout
	Branch string
	Base   string // "" lets gh pick
	Draft  bool
	// TitleFile and BodyFile are the title and body, relative to the
	// checkout; with neither, gh fills both in from the commits.
	TitleFile, BodyFile string
}

// Open opens p with gh, or finds the one already open for its branch (as
// after a resume), and returns its URL, and whether it was already open.
func Open(p PR) (url string, existed bool, err error) {
	// Open ones only: gh pr view also finds a merged or closed PR, and a branch
	// name used again after a merge would then get no PR at all.
	if url, err := run(p.Dir, "gh", "pr", "list", "--head", p.Branch, "--state", "open", "--json", "url", "--jq", ".[0].url"); err == nil && strings.HasPrefix(url, "http") {
		return strings.TrimSpace(url), true, nil
	}

	args := []string{"pr", "create", "--head", p.Branch}
	if p.Base != "" {
		args = append(args, "--base", p.Base)
	}
	if p.Draft {
		args = append(args, "--draft")
	}
	if p.TitleFile == "" && p.BodyFile == "" {
		args = append(args, "--fill")
	} else {
		title, err := os.ReadFile(filepath.Join(p.Dir, p.TitleFile))
		if p.TitleFile == "" || err != nil {
			return "", false, fmt.Errorf("pr_title_file %q isn't in the checkout", p.TitleFile)
		}
		body := filepath.Join(p.Dir, p.BodyFile)
		if _, err := os.Stat(body); p.BodyFile == "" || err != nil {
			return "", false, fmt.Errorf("pr_body_file %q isn't in the checkout", p.BodyFile)
		}
		args = append(args, "--title", strings.TrimSpace(string(title)), "--body-file", body)
	}
	out, err := run(p.Dir, "gh", args...)
	if err != nil {
		return "", false, fmt.Errorf("gh pr create: %s", human.OneLine(out, 300))
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return strings.TrimSpace(lines[len(lines)-1]), false, nil // gh prints the URL last
}

// run runs a command in dir with prf's own environment and returns its
// combined output.
func run(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// ExitCode returns a finished command's exit code: 0 for no error, and 1
// when it didn't run at all.
func ExitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		return 1
	}
	return 0
}
