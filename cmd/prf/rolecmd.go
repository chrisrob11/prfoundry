package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/chrisrob11/prfoundry/internal/atomicfile"
	"github.com/chrisrob11/prfoundry/internal/itemname"
	"github.com/chrisrob11/prfoundry/internal/job"
	"github.com/chrisrob11/prfoundry/internal/jsonio"
	"github.com/chrisrob11/prfoundry/internal/roles"
	"github.com/urfave/cli/v3"
)

// roleEntry describes one role as list and show report it.
type roleEntry struct {
	Name        string `json:"name"`
	Scope       string `json:"scope"` // "repo" or "shared"
	Path        string `json:"path"`
	Description string `json:"description"`
	Overrides   string `json:"overrides,omitempty"` // path of a shared role this repo role hides
	Builtin     bool   `json:"builtin"`             // a built-in role of this name exists
	Valid       bool   `json:"valid"`
	Error       string `json:"error,omitempty"`
	Content     string `json:"content,omitempty"` // show --json only
}

func (a *app) roleCmd() *cli.Command {
	repoFlag := &cli.StringFlag{Name: "repo", Usage: "work on this repo's roles (~/.prf/<repo>/roles) instead of the shared ones"}
	jsonFlag := &cli.BoolFlag{Name: "json", Usage: "print JSON"}
	return &cli.Command{
		Name:  "role",
		Usage: "list, show, and change roles; built for LLMs and scripts as well as people",
		Description: "Roles are <name>.md Go templates that can use {{.PR}}, {{.Branch}}, {{.Task}} and,\n" +
			"for a gate's fix role, {{.Review}}, and for an @ask gate's, {{.Question}}.\n" +
			"Shared roles live in ~/.prf/roles; a repo's own roles in ~/.prf/<repo>/roles\n" +
			"override shared ones of the same name. With --repo, list and show give what that\n" +
			"repo would use, and set and remove change the repo's own roles.\n" +
			"To change one: prf role show <name> > f; edit f; prf role set <name> --file f.\n" +
			"set refuses a role that doesn't parse or renders an empty prompt, and leaves the file unchanged.",
		Commands: []*cli.Command{
			{Name: "list", Usage: "list roles with their scope, path, description and whether each is valid", Flags: []cli.Flag{repoFlag, jsonFlag}, Action: a.roleList},
			{Name: "show", Usage: "print the role prf would use", ArgsUsage: "<name>", Flags: []cli.Flag{repoFlag, jsonFlag}, Action: a.roleShow},
			{Name: "set", Usage: "create or replace a role from --file or stdin, after validating it", ArgsUsage: "<name>",
				Flags: []cli.Flag{repoFlag, &cli.StringFlag{Name: "file", Usage: "read from this file instead of stdin"}}, Action: a.roleSet},
			{Name: "remove", Usage: "delete a role file", ArgsUsage: "<name>", Flags: []cli.Flag{repoFlag}, Action: a.roleRemove},
			{Name: "reset", Usage: "restore a built-in role's original text in the shared roles", ArgsUsage: "<name>", Action: a.roleReset},
		},
	}
}

// listRoles returns the role prf would use for each name, for repo (or
// only the shared roles when repo is empty), sorted by name.
func listRoles(home, repo string) []roleEntry {
	dirs := roles.Dirs(home, repo)
	scopes := []string{"shared"}
	if repo != "" {
		scopes = []string{"repo", "shared"}
	}
	byName := map[string]*roleEntry{}
	for i, dir := range dirs {
		matches, _ := filepath.Glob(filepath.Join(dir, "*.md"))
		for _, path := range matches {
			name := strings.TrimSuffix(filepath.Base(path), ".md")
			if e, ok := byName[name]; ok {
				e.Overrides = path // the repo's role already won
				continue
			}
			byName[name] = loadRoleEntry(name, scopes[i], path)
		}
	}
	entries := make([]roleEntry, 0, len(byName))
	for _, e := range byName {
		entries = append(entries, *e)
	}
	slices.SortFunc(entries, func(a, b roleEntry) int { return strings.Compare(a.Name, b.Name) })
	return entries
}

func loadRoleEntry(name, scope, path string) *roleEntry {
	_, builtinErr := fs.Stat(roles.Builtin(), name+".md")
	e := &roleEntry{Name: name, Scope: scope, Path: path, Builtin: builtinErr == nil}
	b, err := os.ReadFile(path)
	if err == nil {
		e.Content = string(b)
		e.Description = firstLine(e.Content)
		err = roles.Validate(name, e.Content)
	}
	if err != nil {
		e.Error = err.Error()
	}
	e.Valid = err == nil
	return e
}

func findRoleEntry(home, repo, name string) *roleEntry {
	for _, e := range listRoles(home, repo) {
		if e.Name == name {
			return &e
		}
	}
	return nil
}

// firstLine returns text's first non-empty line without markdown heading marks.
func firstLine(text string) string {
	for line := range strings.SplitSeq(text, "\n") {
		if line = strings.TrimSpace(strings.TrimLeft(line, "#")); line != "" {
			return line
		}
	}
	return ""
}

func (a *app) roleList(ctx context.Context, c *cli.Command) error {
	repo, err := optionalRepo(c)
	if err != nil {
		return err
	}
	entries := listRoles(a.home, repo)
	w := c.Root().Writer
	if c.Bool("json") {
		for i := range entries {
			entries[i].Content = ""
		}
		return jsonio.Write(w, entries)
	}
	if len(entries) == 0 {
		_, _ = fmt.Fprintf(w, "no roles in %s (prf config init installs the built-in roles)\n", strings.Join(roles.Dirs(a.home, repo), " or "))
		return nil
	}
	for _, e := range entries {
		status := ""
		if !e.Valid {
			status = "  INVALID: " + e.Error
		}
		if e.Overrides != "" {
			status += "  (overrides " + e.Overrides + ")"
		}
		_, _ = fmt.Fprintf(w, "%-12s %-6s %s%s\n", e.Name, e.Scope, e.Description, status)
	}
	return nil
}

func (a *app) roleShow(ctx context.Context, c *cli.Command) error {
	repo, name, err := repoAndRoleName(c)
	if err != nil {
		return err
	}
	e := findRoleEntry(a.home, repo, name)
	if e == nil {
		return cli.Exit(fmt.Sprintf("prf: no role %q in %s", name, strings.Join(roles.Dirs(a.home, repo), " or ")), job.ExitUsage)
	}
	if c.Bool("json") {
		return jsonio.Write(c.Root().Writer, e)
	}
	_, err = io.WriteString(c.Root().Writer, e.Content)
	return err
}

func (a *app) roleSet(ctx context.Context, c *cli.Command) error {
	repo, name, err := repoAndRoleName(c)
	if err != nil {
		return err
	}
	var text []byte
	if f := c.String("file"); f != "" {
		text, err = os.ReadFile(f)
	} else {
		text, err = io.ReadAll(c.Root().Reader)
	}
	if err != nil {
		return cli.Exit("prf: reading role: "+err.Error(), job.ExitUsage)
	}
	if err := roles.Validate(name, string(text)); err != nil {
		return cli.Exit("prf: not saved: "+err.Error(), job.ExitUsage)
	}
	path := filepath.Join(roles.Dirs(a.home, repo)[0], name+".md")
	if err := atomicfile.Write(path, text); err != nil {
		return cli.Exit("prf: "+err.Error(), job.ExitSetup)
	}
	_, _ = fmt.Fprintf(c.Root().Writer, "saved role %s %s\n", name, path)
	return nil
}

func (a *app) roleRemove(ctx context.Context, c *cli.Command) error {
	repo, name, err := repoAndRoleName(c)
	if err != nil {
		return err
	}
	path := filepath.Join(roles.Dirs(a.home, repo)[0], name+".md")
	if err := os.Remove(path); errors.Is(err, fs.ErrNotExist) {
		return cli.Exit(fmt.Sprintf("prf: no role %q at %s", name, path), job.ExitUsage)
	} else if err != nil {
		return cli.Exit("prf: "+err.Error(), job.ExitSetup)
	}
	w := c.Root().Writer
	_, _ = fmt.Fprintf(w, "removed role %s %s\n", name, path)
	if e := findRoleEntry(a.home, repo, name); e != nil {
		_, _ = fmt.Fprintf(w, "%s now resolves to %s\n", name, e.Path)
	}
	return nil
}

func (a *app) roleReset(ctx context.Context, c *cli.Command) error {
	name, err := nameArg(c, "role")
	if err != nil {
		return err
	}
	b, err := fs.ReadFile(roles.Builtin(), name+".md")
	if err != nil {
		return cli.Exit(fmt.Sprintf("prf: no built-in role %q", name), job.ExitUsage)
	}
	path := filepath.Join(roles.Dirs(a.home, "")[0], name+".md")
	if err := atomicfile.Write(path, b); err != nil {
		return cli.Exit("prf: "+err.Error(), job.ExitSetup)
	}
	_, _ = fmt.Fprintf(c.Root().Writer, "reset role %s %s\n", name, path)
	return nil
}

// optionalRepo returns --repo, which may be empty, after checking it.
func optionalRepo(c *cli.Command) (string, error) {
	if c.String("repo") == "" {
		return "", nil
	}
	return repoArg(c)
}

func repoAndRoleName(c *cli.Command) (repo, name string, err error) {
	if repo, err = optionalRepo(c); err != nil {
		return "", "", err
	}
	name, err = nameArg(c, "role")
	return repo, name, err
}

// nameArg returns the single <name> argument.
func nameArg(c *cli.Command, kind string) (string, error) {
	if c.Args().Len() != 1 {
		return "", cli.Exit(fmt.Sprintf("prf: give exactly one %s name", kind), job.ExitUsage)
	}
	name := c.Args().First()
	if !itemname.Valid(name) {
		return "", cli.Exit(fmt.Sprintf("prf: invalid %s name %q: use letters, digits, '-', '_' and '.'", kind, name), job.ExitUsage)
	}
	return name, nil
}
