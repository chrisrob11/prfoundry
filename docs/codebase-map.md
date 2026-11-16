# Codebase map

Every role is a fresh Claude session, so without help each one would explore the repo from
scratch. Before the first role, prf fills `<job>/context/` once: first the Go codebase map, then
the output of the repo's setup commands.

## The Go codebase map

It takes well under a second (about 80 ms for a 164-package, 2,000-file repo), uses no LLM, and
covers the Go files git tracks that the go tool would build: no tests, nothing under `testdata/`,
`vendor/`, or folders starting with `.` or `_`. Nothing in it is cut short, however big the repo;
it grows by one short entry per package, and the detail is split into a file per package so Claude
opens only what it needs.

- `index.md`, the overview. One entry per package: its doc synopsis, which of the repo's own
  packages it imports, every struct, interface and other type by name, how many functions it
  has, how many of its files are generated, and the file with its details. In a repo with
  several modules, each entry also names its module. Then every `go.mod` (module, Go version,
  direct dependencies), Makefile targets, the files worth reading (README, `CLAUDE.md`,
  `AGENTS.md`, CI, Docker, mise), the directory layout, and the last 10 commits.
- `packages/<dir>.md`, one per package (`packages/internal/db.md`; `packages/_root.md` for the
  root). The package's full doc, every interface, struct (with fields and tags) and other type
  as gofmt'd Go source, and every function and method signature, exported or not, each with
  `file:line`.
- `symbols.json`, for searching rather than reading: every function and type in the repo as one
  JSON table, one row per line. Each file path is stored once, in `files`, keyed by number, and
  each row is `[file, line, declaration]` (signatures folded onto one line, types as
  `type Store struct`). Claude greps it for a name to find where it's defined, then greps
  `"<number>":` for the file:

  ```json
  {
  "columns": ["file", "line", "decl"],
  "files": {
  "0": "cmd/app/main.go",
  "1": "internal/db/db.go"
  },
  "rows": [
  [0,8,"func main()"],
  [1,4,"type Store struct"],
  [1,9,"func New() *Store"]
  ]
  }
  ```

`prf index --dir <checkout> --out <folder>` builds the same map without running anything else, so
you can see what Claude will be given. It defaults to the current directory, writing into
`./codebase-map`.

## Setup commands

The repo's `pipeline.json` `setup` list runs with `sh -c` in the checkout, in order, after
`mise install` and outside the sandbox. The commands get `$PRF_CONTEXT` (the context folder) and
`$PRF_JOB` (the job folder). Anything they write to `$PRF_CONTEXT` is handed to Claude too. A
failing command stops the job at setup (exit 3), before Claude runs. Use them for code generation
(`make generate`) or other indexes:

```json
"setup": [
  "ctags -R -f $PRF_CONTEXT/tags .",
  "go list -json ./... > $PRF_CONTEXT/packages.json"
]
```

## What Claude sees

Every role gets read access to the context folder. Its prompt lists what's at the top of the folder
(`index.md`, `packages/`, and anything setup wrote), not every package file, and says to read
`index.md` first. The map is never written into the repo, so it can't be committed. It's rebuilt
on every run, including `apply`. Set `"index": false` in `pipeline.json` to skip the built-in map,
for example when a setup command writes your own `index.md`.
