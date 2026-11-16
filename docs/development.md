# Development

```sh
make ci       # exactly what CI runs: fmt-check, lint, test, build
make test     # go test -race -count=1 ./... (never cached)
make build    # bin/prf
```

`make lint` and `make fmt` need `golangci-lint`. A PR needs a green `ci` check and the owner's
approval to merge.

The tests run the whole CLI in-process against throwaway git repos and a fake `claude` script that
speaks stream-json, so they need `git` but not Claude, and cost no tokens. To time the codebase
map on a real repo: `PRF_BENCH_DIR=~/code/big go test ./internal/codeindex -run x -bench Build`.

The CLI and the job runner are in `cmd/prf`; self-contained pieces are packages under `internal/`.

| Path | What's in it |
|---|---|
| `cmd/prf/main.go` | Commands, flags, the `job` struct, and how `run` and `apply` assemble a job |
| `cmd/prf/run.go` | Running the roles, progress output, status lines, cloning and switching branches |
| `cmd/prf/stream.go` | Reading Claude's stream-json events into the step feed and result |
| `cmd/prf/state.go` | `state.json` and `prf status` |
| `cmd/prf/costs.go` | `prf costs` |
| `cmd/prf/ship.go` | Pushing and opening the PR after the last step |
| `cmd/prf/lifecycle.go` | `prf resume` and `prf clean` |
| `cmd/prf/config.go` | `app` (the loaded home and `conf.json` every command reads), `config` and `config init` |
| `cmd/prf/roles.go`, `rolecmd.go` | A job's phases from its roles; `prf role` |
| `cmd/prf/pipelinecmd.go` | `prf pipeline`, and checking a pipeline's roles |
| `cmd/prf/review.go`, `review.html` | The `@review` gate and its review page |
| `cmd/prf/ask.go` | The `@ask` gate |
| `cmd/prf/check.go` | The `@check` gate |
| `cmd/prf/limit.go` | Waiting out Claude's usage limit (the `paused` state) |
| `cmd/prf/context.go` | Preparing `<job>/context`: setup commands and `prf index` |
| `internal/config` | `conf.json`: its types, reading and checking it, the default home, `~` expansion |
| `internal/jobenv` | `job_env` and `job_seed`: the job's `mise.toml`, seed copies, and the folders steps may write |
| `internal/sandbox` | The Claude Code sandbox settings file: its defaults and where it's found |
| `internal/mise` | mise's environment and trust, and installing a repo's tools |
| `internal/pipeline` | Steps, gates and `pipeline.json` |
| `internal/roles` | The built-in roles (`builtin/*.md`, installed by `config init`), role lookup and prompt rendering |
| `internal/codeindex` | The codebase map |
| `internal/excerpt` | Picking out what failed from a check's output |
| `internal/atomicfile` | Writing a file atomically |
| `internal/textfile` | Writing a file in place, making its folder first |
| `internal/jsonio` | Strict JSON decoding and indented JSON output |
| `internal/gitx` | Running git: for its effect, its success, or its output |
| `internal/itemname` | What a role, pipeline or repo may be called |
