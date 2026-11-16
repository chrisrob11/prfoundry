// Package roles owns prf's roles: the built-in role files, where a repo's
// roles are looked up, and rendering a role's template into a prompt.
package roles

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
)

//go:embed builtin/*.md
var builtin embed.FS

// Builtin is the built-in roles, as <name>.md files at its root; `prf
// config init` installs them into home/roles.
func Builtin() fs.FS {
	sub, err := fs.Sub(builtin, "builtin")
	if err != nil {
		panic(err) // the embedded directory always exists
	}
	return sub
}

// roleName is what a role may be called. Role names appear in
// phase=<name> status lines.
var roleName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// Data is what role templates can reference. Context is not for
// templates: Render lists those files in every prompt. Review is set only
// for a gate's fix role: the comments to address, or the answers to record.
// Question is set only for an ask gate's: the question they answer.
// Answers is not for templates either: when an ask gate re-runs the step
// that asked, Render adds the questions it asked and your answers.
// PriorRun is set only for a role step that ran before in this job: the
// commit it ended at. PriorReport is that run's final message, saved in a
// file, if it left one. Render adds a note about both.
type Data struct {
	PR, Branch, Task, Review, Question string
	Context                            []string
	Answers                            string
	PriorRun, PriorReport              string
}

// Dirs returns where roles are looked up, in order: the repo's own
// (home/<repo>/roles), then the shared ones (home/roles). With no repo,
// only the shared ones.
func Dirs(home, repo string) []string {
	shared := filepath.Join(home, "roles")
	if repo == "" {
		return []string{shared}
	}
	return []string{filepath.Join(home, repo, "roles"), shared}
}

// Find returns the text of <name>.md from the first of dirs that has it.
func Find(dirs []string, name string) (string, error) {
	if !roleName.MatchString(name) {
		return "", fmt.Errorf("invalid role name %q: use letters, digits, '-', '_' and '.'", name)
	}
	for _, dir := range dirs {
		b, err := os.ReadFile(filepath.Join(dir, name+".md"))
		if err == nil {
			return string(b), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
	}
	return "", fmt.Errorf("no role %q in %s (prf config init installs the built-in roles)", name, strings.Join(dirs, " or "))
}

// Parse parses a role's text as its template.
func Parse(name, text string) (*template.Template, error) {
	tmpl, err := template.New(name).Option("missingkey=error").Parse(text)
	if err != nil {
		return nil, fmt.Errorf("role %s: %w", name, err)
	}
	return tmpl, nil
}

// Validate checks that text parses and renders to a non-empty prompt
// with every mix of PR and task, so a saved role can't break a run later.
func Validate(name, text string) error {
	tmpl, err := Parse(name, text)
	if err != nil {
		return err
	}
	for _, data := range []Data{
		{Branch: "b"},
		{Branch: "b", Review: "## Review"},
		{Branch: "b", Review: "## Answers", Question: "## Question"},
		{Branch: "b", PR: "/spec.md"},
		{Branch: "b", Task: "task"},
		{Branch: "b", PR: "/spec.md", Task: "task"},
		{Branch: "b", PriorRun: "abc1234", PriorReport: "/report.md"},
	} {
		if _, err := Render(tmpl, data); err != nil {
			return fmt.Errorf("role %s with PR=%q and task=%q: %w", name, data.PR, data.Task, err)
		}
	}
	return nil
}

// Render returns the prompt for one phase: the role's template tmpl (or,
// with a nil tmpl, the task), the codebase context files, and the task. It fails if the
// role's own text is empty, whatever gets added to it.
func Render(tmpl *template.Template, data Data) (string, error) {
	var b strings.Builder
	if tmpl == nil {
		b.WriteString(data.Task)
	} else if err := tmpl.Execute(&b, data); err != nil {
		return "", err
	}
	if strings.TrimSpace(b.String()) == "" {
		return "", errors.New("prompt is empty")
	}
	if len(data.Context) > 0 {
		b.WriteString("\n\n## Codebase context\n\nBefore this run, prf prepared these files about the codebase. " +
			"Read index.md first instead of exploring from scratch; it lists every package and says which file " +
			"under packages/ has that package's types and function signatures. To find where a function or type " +
			"is defined, grep symbols.json: one [file, line, declaration] row per symbol, with the file as a number " +
			"you look up in its \"files\" map. " +
			"Use the rest as needed:\n\n")
		for _, f := range data.Context {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}
	if tmpl != nil && data.Task != "" {
		fmt.Fprintf(&b, "\n\n## Task\n\n%s\n", data.Task)
	}
	if data.PriorRun != "" {
		report := ""
		if data.PriorReport != "" {
			report = fmt.Sprintf(" and its earlier report (%s)", data.PriorReport)
		}
		fmt.Fprintf(&b, "\n\n## This step ran before\n\nThis step ran before, ending at %s: read `git diff %s..HEAD`%s first, "+
			"and read unchanged code only where you need it.\n", data.PriorRun, data.PriorRun, report)
	}
	if data.Answers != "" {
		fmt.Fprintf(&b, "\n\n## Your questions, answered\n\nAn earlier run of this step stopped to ask the owner. "+
			"Here is what it asked and what the owner answered. Follow the answers, and don't ask these again.\n\n%s", data.Answers)
	}
	return b.String(), nil
}
