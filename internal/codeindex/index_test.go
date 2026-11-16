package codeindex

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// newGoRepo makes a small committed Go module plus an untracked file.
func newGoRepo(t *testing.T) string {
	t.Helper()
	isolateGit(t)
	dir := filepath.Join(t.TempDir(), "demo")
	files := map[string]string{
		"go.mod":    "module example.com/demo\n\ngo 1.26\n\nrequire (\n\tgithub.com/a/b v1.0.0\n\tgithub.com/c/d v1.0.0 // indirect\n)\n",
		"README.md": "# Demo\n\n```sh\ngo run ./cmd/app\n```\n",
		"Makefile":  ".PHONY: test\nVERSION := 1\ntest:\n\tgo test ./...\nbuild: test\n\tgo build ./...\n",
		"internal/db/db.go": "// Package db stores things. It has more to say.\npackage db\n\n" +
			"type Store struct{ Name string; conn *row }\ntype row struct{}\ntype Getter interface{ Get() error }\ntype ID int\n\n" +
			"func New() *Store { return nil }\nfunc (s *Store) Get() {}\nfunc (r row) Hidden() {}\nfunc helper() {}\n",
		"internal/db/db_test.go":   "package db\n\nfunc TestOnly() {}\n",
		"cmd/app/main.go":          "package main\n\nimport (\n\t\"fmt\"\n\t\"example.com/demo/internal/db\"\n)\n\nfunc main() { fmt.Println(db.New()) }\nfunc run() {}\n",
		".github/workflows/ci.yml": "on: push\n",
		"docs/design notes.md":     "# notes\n",
	}
	for name, body := range files {
		writeFileT(t, filepath.Join(dir, name), body)
	}
	mustGit(t, "", "init", "-q", "-b", "main", dir)
	mustGit(t, dir, "add", "-A")
	mustGit(t, dir, "commit", "-q", "-m", "first commit")
	writeFileT(t, filepath.Join(dir, "untracked.go"), "package demo\n\nfunc Secret() {}\n")
	return dir
}

func TestBuild(t *testing.T) {
	dir := newGoRepo(t)
	out := t.TempDir()
	if err := Build(dir, "demo", "feat/x", out); err != nil {
		t.Fatal(err)
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	index, db, app := read("index.md"), read("packages/internal/db.md"), read("packages/cmd/app.md")

	for _, want := range []string{
		"# Codebase map: demo",
		"branch feat/x",
		"## Go packages (2 packages, 2 structs, 1 interfaces, 6 functions)",
		"### `internal/db` (package db)\n\nPackage db stores things.\n\n" +
			"- structs: Store, row\n- interfaces: Getter\n- other types: ID\n- 4 functions; details: packages/internal/db.md\n",
		"### `cmd/app` (package main)\n\n- imports from this repo: internal/db\n- 2 functions; details: packages/cmd/app.md\n",
		"- module: example.com/demo",
		"- direct dependencies: github.com/a/b\n",
		"### Makefile targets\n\ntest, build",
		"README.md", ".github/workflows/ci.yml",
		"## Layout (8 tracked files, 3 at the top level)",
		"- `docs/` 1 files",
		"  - `internal/db/` 2 files",
		"first commit",
	} {
		if !strings.Contains(index, want) {
			t.Errorf("index.md missing %q", want)
		}
	}
	for _, want := range []string{
		"# Package db (`internal/db`)\n\nPackage db stores things. It has more to say.\n\n",
		"## Interfaces\n\n### Getter (internal/db/db.go:6)\n\n```go\ntype Getter interface{ Get() error }\n```",
		"## Structs\n\n### Store (internal/db/db.go:4)\n\n```go\ntype Store struct {\n\tName string\n\tconn *row\n}\n```",
		"### row (internal/db/db.go:5)",
		"## Other types\n\n### ID (internal/db/db.go:7)\n\n```go\ntype ID int\n```",
		"// internal/db/db.go:9\nfunc New() *Store\n",
		"// internal/db/db.go:10\nfunc (s *Store) Get()\n",
		"func (r row) Hidden()",
		"func helper()",
	} {
		if !strings.Contains(db, want) {
			t.Errorf("packages/internal/db.md missing %q", want)
		}
	}
	symbols := read(symbolsFile)
	var table struct {
		Columns []string          `json:"columns"`
		Files   map[string]string `json:"files"`
		Rows    [][3]any          `json:"rows"`
	}
	if err := json.Unmarshal([]byte(symbols), &table); err != nil {
		t.Fatalf("symbols.json isn't valid JSON: %v\n%s", err, symbols)
	}
	if !slices.Equal(table.Columns, []string{"file", "line", "decl"}) || len(table.Rows) != 10 {
		t.Errorf("want columns file, line, decl and 10 rows (6 functions, 4 types): %+v", table)
	}
	var got []string
	for _, r := range table.Rows {
		got = append(got, fmt.Sprintf("%s:%v  %s", table.Files[fmt.Sprint(r[0])], r[1], r[2]))
	}
	for _, want := range []string{
		"cmd/app/main.go:8  func main()",
		"internal/db/db.go:4  type Store struct",
		"internal/db/db.go:6  type Getter interface",
		"internal/db/db.go:7  type ID int",
		"internal/db/db.go:9  func New() *Store",
		"internal/db/db.go:10  func (s *Store) Get()",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("symbols missing %q; have %v", want, got)
		}
	}
	// One row and one file entry per line, so grep finds them.
	for _, line := range []string{"\n[1,9,\"func New() *Store\"]", "\n\"1\": \"internal/db/db.go\""} {
		if !strings.Contains(symbols, line) {
			t.Errorf("symbols.json should have %q on its own line:\n%s", line, symbols)
		}
	}
	if !strings.Contains(app, "Imports from this repo: internal/db") || !strings.Contains(app, "func run()") {
		t.Errorf("packages/cmd/app.md:\n%s", app)
	}
	for _, unwanted := range []string{"Secret", "untracked.go", "TestOnly", "github.com/c/d", "VERSION", "return nil", "Println"} {
		if strings.Contains(index+db+app, unwanted) {
			t.Errorf("map should not mention %q", unwanted)
		}
	}
	if t.Failed() {
		t.Log(index, db, app)
	}
}

// A repo far bigger than any old cap is mapped in full.
func TestBuildDoesNotTruncate(t *testing.T) {
	isolateGit(t)
	dir := filepath.Join(t.TempDir(), "big")
	writeFileT(t, filepath.Join(dir, "go.mod"), "module example.com/big\n\ngo 1.26\n")
	const pkgs, structsPer = 300, 40
	for i := range pkgs {
		var src strings.Builder
		fmt.Fprintf(&src, "package p%d\n\n", i)
		for j := range structsPer {
			fmt.Fprintf(&src, "type LongStructName%dWithManyFields%d struct {\n\tFieldOne, FieldTwo, FieldThree string\n\tFieldFour map[string][]int\n}\n\n", i, j)
		}
		writeFileT(t, filepath.Join(dir, "pkg", fmt.Sprintf("p%03d", i), "p.go"), src.String())
	}
	mustGit(t, "", "init", "-q", "-b", "main", dir)
	mustGit(t, dir, "add", "-A")
	mustGit(t, dir, "commit", "-q", "-m", "big")

	out := t.TempDir()
	if err := Build(dir, "big", "main", out); err != nil {
		t.Fatal(err)
	}
	index, _ := os.ReadFile(filepath.Join(out, "index.md"))
	if len(index) < 200<<10 {
		t.Fatalf("test repo too small to exceed the old 200 KB cap: index.md is %d bytes", len(index))
	}
	if strings.Contains(string(index), "cut off") || !strings.Contains(string(index), "LongStructName299WithManyFields39") {
		t.Error("index.md is missing the last package's last struct")
	}
	last, err := os.ReadFile(filepath.Join(out, "packages", "pkg", "p299.md"))
	if err != nil || !strings.Contains(string(last), "type LongStructName299WithManyFields39 struct") {
		t.Errorf("last package file incomplete: %v", err)
	}
}

func TestBuildNoFiles(t *testing.T) {
	isolateGit(t)
	dir := t.TempDir()
	mustGit(t, dir, "init", "-q")
	if err := Build(dir, "empty", "main", filepath.Join(t.TempDir(), "index.md")); err == nil {
		t.Error("want an error for a repo with no tracked files")
	}
}

func TestIndexHelpers(t *testing.T) {
	if got := oneLineDecl("func Long(\n\ta int,\n\tb string,\n) error"); got != "func Long( a int, b string, ) error" {
		t.Errorf("oneLineDecl = %q", got)
	}
	for src, want := range map[string]string{
		"type Store struct {\n\tName string\n}":         "type Store struct",
		"type Empty struct{}":                           "type Empty struct",
		"type Getter interface{ Get() error }":          "type Getter interface",
		"type Set[T comparable] struct{ m map[T]bool }": "type Set[T comparable] struct",
		"type Handler func(w io.Writer) error":          "type Handler func(w io.Writer) error",
	} {
		if got := typeHeader(src); got != want {
			t.Errorf("typeHeader(%q) = %q, want %q", src, got, want)
		}
	}
	if got := firstSentence("Package x does a thing. Then more.\n\nSecond paragraph."); got != "Package x does a thing." {
		t.Errorf("firstSentence = %q", got)
	}
	mods := []goMod{{dir: "tools", path: "example.com/demo/tools"}, {dir: ".", path: "example.com/demo"}}
	for imp, want := range map[string]string{
		"example.com/demo":              ".",
		"example.com/demo/internal/x":   "internal/x",
		"example.com/demo/tools":        "tools",
		"example.com/demo/tools/lint/y": "tools/lint/y",
		"example.com/demox":             "",
		"fmt":                           "",
	} {
		if got := internalImport(mods, imp); got != want {
			t.Errorf("internalImport(%q) = %q, want %q", imp, got, want)
		}
	}
	for f, want := range map[string]bool{
		"internal/db/db.go": false, "integration/testdata/x.go": true, "vendor/a/b.go": true,
		"_old/x.go": true, ".hidden/x.go": true, "pkg/x_y.go": false,
	} {
		if got := goIgnored(f); got != want {
			t.Errorf("goIgnored(%q) = %v, want %v", f, got, want)
		}
	}
	if got := makeTargets("all: build\n\nbuild:\n\tgo build\nX = 1\nbuild: more\n"); !slices.Equal(got, []string{"all", "build"}) {
		t.Errorf("makeTargets = %v", got)
	}
	for dir, want := range map[string]string{".": "packages/_root.md", "internal/db": "packages/internal/db.md"} {
		if got := filepath.ToSlash(packageFile(dir)); got != want {
			t.Errorf("packageFile(%q) = %q, want %q", dir, got, want)
		}
	}
}

func TestBuildModules(t *testing.T) {
	dir := newGoRepo(t)
	for name, body := range map[string]string{
		"tools/go.mod":              "module example.com/demo/tools\n\ngo 1.26\n",
		"tools/lint/lint.go":        "package lint\n\nimport (\n\t\"example.com/demo/internal/db\"\n\t\"example.com/demo/tools/rules\"\n)\n\nvar _ = db.New\nvar _ = rules.X\n",
		"tools/rules/rules.go":      "package rules\n\nvar X = 1\n",
		"integration/testdata/x.go": "package fixture\n\ntype Fixture struct{}\n",
		"internal/db/gen.go":        "// Code generated by hand. DO NOT EDIT.\n\npackage db\n\ntype Gen struct{}\n",
	} {
		writeFileT(t, filepath.Join(dir, name), body)
	}
	mustGit(t, dir, "add", "tools", "integration", "internal")
	mustGit(t, dir, "commit", "-q", "-m", "modules")

	out := t.TempDir()
	if err := Build(dir, "demo", "main", out); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(out, "index.md"))
	index := string(b)
	for _, want := range []string{
		"### `tools/lint` (package lint)\n\n- module: example.com/demo/tools\n- imports from this repo: internal/db, tools/rules\n",
		"### `internal/db` (package db)\n\nPackage db stores things.\n\n- module: example.com/demo\n- 1 generated files\n",
		"### go.mod\n\n- module: example.com/demo",
		"### tools/go.mod\n\n- module: example.com/demo/tools",
	} {
		if !strings.Contains(index, want) {
			t.Errorf("index.md missing %q", want)
		}
	}
	if strings.Contains(index, "Fixture") || strings.Contains(index, "package fixture") {
		t.Error("testdata should not be indexed")
	}
	if strings.Index(index, "### go.mod") > strings.Index(index, "### tools/go.mod") {
		t.Error("root go.mod should be listed first")
	}
	if t.Failed() {
		t.Log(index)
	}
}

// isolateGit keeps the user's git config out of a test's git commands.
func isolateGit(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for k, v := range map[string]string{
		"GIT_CONFIG_COUNT": "2",
		"GIT_CONFIG_KEY_0": "gc.auto", "GIT_CONFIG_VALUE_0": "0",
		"GIT_CONFIG_KEY_1": "maintenance.auto", "GIT_CONFIG_VALUE_1": "false",
	} {
		t.Setenv(k, v)
	}
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "prf test")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "test@example.com")
	}
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeFileT(t *testing.T, path, s string) {
	t.Helper()
	if err := writeFile(path, s); err != nil {
		t.Fatal(err)
	}
}
