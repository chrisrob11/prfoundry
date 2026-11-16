// Package gitx runs git commands in a folder: for their effect, for
// whether they succeed, or for their output.
package gitx

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Run runs git in dir, discarding stdout and passing stderr through.
func Run(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = io.Discard, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w", args[0], err)
	}
	return nil
}

// OK reports whether a git command succeeds, discarding its output.
func OK(dir string, args ...string) bool {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	return cmd.Run() == nil
}

// Output returns a git command's trimmed stdout, or "" on failure.
func Output(dir string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	b, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
