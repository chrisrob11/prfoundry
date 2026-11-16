// Package excerpt cuts a failed command's output down to what a fix role
// needs: the failures in Go's output formats, or the output's tail.
package excerpt

import (
	"fmt"
	"regexp"
	"strings"
)

// A failed command's output can be long, and the failure isn't always at its
// end: a big test suite prints its first failure long before it finishes. So
// a fix role gets an excerpt. For output in Go's formats (build and vet
// errors, failed tests, panics, data races, linters' file:line reports),
// that's just the failures, wherever they are; for anything else, its tail.
// The full output is saved for the fix role to read.

// How much an excerpt keeps.
const (
	perBlock   = 60  // lines of one failed test, panic or race report, and of errors
	maxBlocks  = 20  // failed tests, panics and race reports
	totalLines = 400 // lines in all, from the top
)

var (
	posLine   = regexp.MustCompile(`^\S+\.go:\d+(:\d+)?: `)              // a compiler, vet or linter error
	failLine  = regexp.MustCompile(`^\s*--- FAIL: (\S+)`)                // a failed test
	passLine  = regexp.MustCompile(`^\s*--- (PASS|SKIP): `)              // a test that didn't fail
	runLine   = regexp.MustCompile(`^=== (RUN|CONT|PAUSE|NAME)\s+(\S+)`) // go test -v: whose output follows
	pkgLine   = regexp.MustCompile(`^(FAIL|ok)\s+\S+`)                   // a package's result
	goroutine = regexp.MustCompile(`^goroutine \d+ \[`)
)

// Failures returns the parts of out that say what failed, at most about
// maxBytes long, and false if out isn't in a format it knows, in which case
// the caller falls back to its Tail.
func Failures(out string, maxBytes int) (string, bool) {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	var errs, pkgs []string
	var blocks [][]string
	seen := map[string]bool{}
	logs := map[string][]string{} // go test -v: each test's log, by the header it came under
	current := ""                 // the test whose log -v is printing
	extra := 0                    // blocks left out
	add := func(block []string) {
		if len(blocks) == maxBlocks {
			extra++
			return
		}
		if len(block) > perBlock {
			n := len(block) - perBlock
			block = append(block[:perBlock:perBlock], fmt.Sprintf("    ... %d more lines", n))
		}
		blocks = append(blocks, block)
	}
	// ends is whether a line starts something else, so a block that never
	// closed (output cut off by a timeout) stops there.
	ends := func(l string) bool { return failLine.MatchString(l) || pkgLine.MatchString(l) }

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		switch {
		case posLine.MatchString(line):
			if !seen[line] {
				seen[line] = true
				errs = append(errs, line)
			}
		case strings.HasPrefix(line, "WARNING: DATA RACE"):
			end := i + 1
			for end < len(lines) && !strings.HasPrefix(lines[end], "==================") && !ends(lines[end]) {
				end++
			}
			add(lines[i:end])
			i = end - 1
		case strings.HasPrefix(line, "panic: ") || strings.HasPrefix(line, "fatal error: "):
			end := i + 1 // to the end of the first goroutine's stack
			for end < len(lines) && !goroutine.MatchString(lines[end]) && !ends(lines[end]) {
				end++
			}
			if end < len(lines) && goroutine.MatchString(lines[end]) {
				for end < len(lines) && strings.TrimSpace(lines[end]) != "" {
					end++
				}
			}
			add(lines[i:end])
			i = end - 1
		case runLine.MatchString(line):
			if m := runLine.FindStringSubmatch(line); m[1] != "PAUSE" {
				current = m[2]
			}
		case failLine.MatchString(line):
			name := failLine.FindStringSubmatch(line)[1]
			block := append([]string{line}, logs[name]...) // with -v, its log came before
			end := i + 1                                   // without, it comes after, indented
			for end < len(lines) && strings.HasPrefix(lines[end], "    ") && !failLine.MatchString(lines[end]) && !passLine.MatchString(lines[end]) {
				end++
			}
			add(append(block, lines[i+1:end]...))
			i, current = end-1, ""
		case passLine.MatchString(line):
			current = ""
		case pkgLine.MatchString(line):
			if strings.HasPrefix(line, "FAIL") {
				pkgs = append(pkgs, line)
			}
			current = ""
		case current != "" && strings.HasPrefix(line, "    "):
			logs[current] = append(logs[current], line)
		}
	}
	if len(errs) == 0 && len(blocks) == 0 {
		return "", false
	}

	var parts []string
	if len(errs) > 0 {
		shown := errs[:min(len(errs), perBlock)]
		part := strings.Join(shown, "\n")
		if n := len(errs) - len(shown); n > 0 {
			part += fmt.Sprintf("\n... %d more errors", n)
		}
		parts = append(parts, part)
	}
	if len(pkgs) > 0 {
		parts = append(parts, strings.Join(pkgs, "\n"))
	}
	for _, block := range blocks {
		parts = append(parts, strings.Join(block, "\n"))
	}
	if extra > 0 {
		parts = append(parts, fmt.Sprintf("... %d more failures", extra))
	}
	return head(strings.Join(parts, "\n\n"), totalLines, maxBytes), true
}

// head returns the start of s, at most maxLines lines and about maxBytes,
// saying how much it left out: an excerpt's first failures matter most.
func head(s string, maxLines, maxBytes int) string {
	lines := strings.Split(s, "\n")
	kept, size := 0, 0
	for kept < len(lines) && kept < maxLines && size+len(lines[kept])+1 <= maxBytes {
		size += len(lines[kept]) + 1
		kept++
	}
	if kept == len(lines) {
		return s
	}
	return strings.Join(lines[:kept], "\n") + fmt.Sprintf("\n... %d more lines", len(lines)-kept)
}

// Tail returns the end of s: its last lines, at most maxLines of them and
// maxBytes long, and whether that left anything out.
func Tail(s string, maxLines, maxBytes int) (string, bool) {
	cut := false
	if len(s) > maxBytes {
		s, cut = s[len(s)-maxBytes:], true
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:] // from a whole line
		}
	}
	if lines := strings.Split(s, "\n"); len(lines) > maxLines {
		s, cut = strings.Join(lines[len(lines)-maxLines:], "\n"), true
	}
	return s, cut
}
