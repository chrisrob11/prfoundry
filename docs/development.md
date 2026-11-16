# Development

```sh
make ci       # exactly what CI runs: fmt-check, lint, test, build
make test     # go test -race ./...
make build    # bin/prf
```

`make lint` and `make fmt` need `golangci-lint`. A PR needs a green `ci` check and the owner's
approval to merge.

The tests run the whole CLI in-process against throwaway git repos and a fake `claude` script that
speaks stream-json, so they need `git` but not Claude, and cost no tokens. To time the codebase
map on a real repo: `PRF_BENCH_DIR=~/code/big go test ./cmd/prf -run x -bench BuildIndex`.

The code is in `cmd/prf`:

| File | What's in it |
|---|---|
| `main.go` | Commands, flags, and how `run` and `apply` assemble a job |
| `run.go` | Running the roles, progress output, status lines, git helpers |
| `stream.go` | Reading Claude's stream-json events into the step feed and result |
| `state.go` | `state.json` and `prf status` |
| `costs.go` | `prf costs` |
| `ship.go` | Pushing and opening the PR after the last step |
| `lifecycle.go` | `prf resume` and `prf clean` |
| `config.go` | The home folder, `conf.json`, `config` and `config init` |
| `roles.go`, `rolecmd.go` | Loading and validating roles; `prf role` |
| `pipeline.go` | `pipeline.json` and `prf pipeline` |
| `review.go`, `review.html` | The `@review` gate and its review page |
| `ask.go` | The `@ask` gate |
| `check.go`, `excerpt.go` | The `@check` gate, and picking out what failed from its output |
| `limit.go` | Waiting out Claude's usage limit (the `paused` state) |
| `context.go`, `index.go` | Preparing `<job>/context`: setup commands, the codebase map, and `prf index` |
| `sandbox.go`, `tools.go` | The sandbox settings file; mise install and trust |
| `jobenv.go` | `job_env` and `job_seed`: the job's `mise.toml`, seed copies, and the folders steps may write |
| `roles/*.md` | The built-in roles, installed by `config init` |
