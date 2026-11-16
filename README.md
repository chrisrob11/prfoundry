# prfoundry

`prf` takes a PR spec and runs it through a pipeline of headless Claude Code roles (implement,
test, review, ...) in its own clone and branch, so you can run a whole wave of PRs at once. Gates let a pipeline run real checks
(`@check`), stop for your review (`@review`), or ask you a question (`@ask`) before it carries on,
and prf itself pushes the branch and opens the PR only once every step has passed.

It's built to be the "hands" for an orchestrator, a person or a small local LLM working through a
plan: one `prf run` per PR, each reporting a machine-readable status when it's done.

## Requirements

- **claude or opencode** on your PATH and logged in: Claude Code (the default, run as `claude -p`)
  or [opencode](docs/configuration.md#the-agent-claude-or-opencode) (`opencode run`).
- **git**, and **gh** if you want prf to open PRs (`--pr-create`).
- **Go** to build prf. The repo pins its version in `mise.toml`.
- **For the sandbox:** macOS works out of the box. Linux and WSL2 need `bubblewrap` and `socat`
  (see [Claude Code's sandboxing docs](https://code.claude.com/docs/en/sandboxing)).
- **For `@check` in a sandboxed job:** `srt`, Anthropic's sandbox runtime:
  `npm install -g @anthropic-ai/sandbox-runtime`.
- **Optional:** mise, if your repos use it for tool versions.

## Install

```sh
go install github.com/chrisrob11/prfoundry/cmd/prf@latest
```

Or build from source:

```sh
git clone git@github.com:chrisrob11/prfoundry.git && cd prfoundry
mise install          # the Go version this repo pins
make build            # writes bin/prf; copy it to any folder on your PATH
```

Under mise, `go install` puts the binary in a folder tied to one Go version, which goes away when
you upgrade Go; copying `bin/prf` to a folder like `~/.local/bin` avoids that. To upgrade, run the
same install again.

## Quick start

```sh
# 1. Set up ~/.prf: conf.json, the built-in roles, and the sandbox settings.
#    Safe to re-run: it only writes files that don't exist yet.
prf config init

# 2. Write a PR spec: a markdown file describing the change.
$EDITOR ~/plans/login.md

# 3. Run it: clone the repo, create branch prf/login, then implement -> test -> review.
#    Progress prints as it goes; add -v to see every step Claude takes.
prf run --repo git@github.com:me/app.git --pr ~/plans/login.md

# 4. From another terminal, or afterwards: where is every job?
prf status

# Watch everything in your browser: every job, its log, its diff, and what's waiting for you.
prf ui
```

The result is a branch in the job's clone, with each role's commits on it:

```sh
git -C ~/prf-work/app/login/repo log --oneline
```

Claude's summary of each role is printed when it finishes and kept in
`~/prf-work/app/login/claude.log`. Nothing is pushed unless you ask: add `--push --pr-create draft`
and prf pushes the branch and opens a draft PR once every step passes.

To shorten the command, add a repo alias and a PR folder to `~/.prf/conf.json`:

```json
{"pr_dir": "~/plans", "repos": {"app": "git@github.com:me/app.git"}}
```

Then step 3 is `prf run --repo app --pr login`. To give a repo its own path through the roles:

```sh
prf role set perf-test --repo app < perf-test.md       # a role only this repo uses
prf pipeline set feature --repo app --roles implement:opus,perf-test,test,review:haiku
prf run --repo app --pr login                          # feature is now app's default
```

## Documentation

| Page | What it covers |
|---|---|
| [How it works](docs/how-it-works.md) | How a job runs, what Claude is given, the job's tmp folder and environment |
| [Configuration](docs/configuration.md) | The prf home, `conf.json`, `pipeline.json`, and environment variables |
| [Roles and pipelines](docs/roles-and-pipelines.md) | Which roles and models run, what a step must leave behind, the built-in roles, writing and managing roles |
| [Gates](docs/gates.md) | `@review` (human review), `@ask` (questions from a step) and `@check` (real checks) |
| [Codebase map](docs/codebase-map.md) | The map prf builds for every job, and the repo's setup commands |
| [Monitoring](docs/monitoring.md) | The job folder, progress output, `prf status`, `prf ui`, `prf costs`, status lines |
| [Orchestrating](docs/orchestrating.md) | Running waves of PRs, pushing and opening PRs, cost caps, exit codes, resume and clean |
| [Sandbox](docs/sandbox.md) | What Claude can reach by default, how to change it, and mise |
| [Command reference](docs/commands.md) | Every command and flag |
| [Troubleshooting](docs/troubleshooting.md) | Common errors and their fixes |
| [Development](docs/development.md) | How the tests work, and a map of the source files |

## Development

```sh
make ci      # exactly what CI runs: fmt-check, lint, test with a coverage floor, build
```

Run it before pushing. A PR needs a green `ci` check and the owner's approval to merge. See
[docs/development.md](docs/development.md) for how the tests work and where the code lives.
See [CONTRIBUTING.md](CONTRIBUTING.md) to contribute and [SECURITY.md](SECURITY.md) to report a vulnerability.

## License

MIT; see [LICENSE](LICENSE).
