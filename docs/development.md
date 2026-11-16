# Development

```sh
make ci       # exactly what CI runs: fmt-check, lint, cover-check, build
make test     # go test -race -count=1 ./... (never cached)
make cover    # the tests, writing coverage.out, and the total statement coverage
make build    # bin/prf
```

`make lint` and `make fmt` need `golangci-lint`. A PR needs a green `ci` check and the owner's
approval to merge. `make cover-check` fails when total statement coverage falls below
`COVER_FLOOR` in the Makefile; raise the floor when coverage rises, never lower it to pass.

The tests run the whole CLI in-process against throwaway git repos and a fake `claude` script that
speaks stream-json, so they need `git` but not Claude, and cost no tokens. They watch a job, or speed
it up, through `job.Runtime` and the app's stderr, not package variables. To time the codebase
map on a real repo: `PRF_BENCH_DIR=~/code/big go test ./internal/codeindex -run x -bench Build`.

The CLI is in `cmd/prf`: it turns flags, `conf.json` and `pipeline.json` into a `job.Options`,
and runs the job. Everything else is a package under `internal/`.

| Path | What's in it |
|---|---|
| `cmd/prf/main.go` | Commands and flags, and how `run` and `apply` build a job's options |
| `cmd/prf/status.go`, `costs.go` | `prf status` and `prf costs` |
| `cmd/prf/lifecycle.go` | `prf resume` and `prf clean` |
| `cmd/prf/config.go` | `app` (the loaded home and `conf.json` every command reads), `config` and `config init` |
| `cmd/prf/rolecmd.go`, `pipelinecmd.go` | `prf role`; `prf pipeline`, and checking a pipeline's roles |
| `cmd/prf/indexcmd.go` | `prf index` |
| `internal/job` | One job: `Run` (clone, then set up), `Apply` (an existing checkout), `Resume`; the phase loop and status lines (`run.go`), setup (`setup.go`), the gates (`gate_check.go`, `gate_review.go`, `gate_ask.go`), the usage-limit pause, `<job>/context`, shipping |
| `internal/agent` | What a job needs from a coding agent: a run's command, its output's progress and result |
| `internal/claude` | Claude Code as an agent: a `claude -p` call's arguments, reading its stream-json, and its usage-limit error |
| `internal/state` | `state.json`: writing it, finding and listing jobs, and summing their spend |
| `internal/review` | The review page the `@review` and `@ask` gates serve (`review.html`), and their saved rounds |
| `internal/ship` | Pushing the branch and opening its PR |
| `internal/human` | A text's first line, durations and dollars, for progress lines and tables |
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

## Releasing

Releases are built by GoReleaser (`.goreleaser.yaml`) from a version tag. To cut one, tag a commit
on `main` and push the tag:

```sh
git switch main && git pull
git tag v0.1.0
git push origin v0.1.0
```

The `release` workflow then builds `prf` for darwin and linux (amd64 and arm64), stamps the version,
commit and date into it, and publishes tar.gz archives, `checksums.txt` and a changelog of the
`feat` and `fix` commits as a GitHub release. To try the config locally without publishing:
`go run github.com/goreleaser/goreleaser/v2@latest release --snapshot --clean` (writes `dist/`).
