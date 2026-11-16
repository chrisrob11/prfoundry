# prfoundry

A tiny CLI that runs Claude Code headless on a Go repo, through a pipeline of roles, and waits
until it finishes. It's built to be the "hands" for a local orchestrator (for example, opencode
with a small LLM) that reads a plan and starts a wave of PRs: one `prf run` per PR, each in its
own clone and branch, each reporting a machine-readable status when it's done.

- `prf run` clones a repo into a fresh job folder, creates a branch, and runs a pipeline there.
- `prf apply` runs a pipeline, or a single prompt, in a checkout you already have.
- `prf status` shows every job: which step of its pipeline it's on and what Claude is doing.
- `prf role` and `prf pipeline` list, show and change roles and pipelines, with validation,
  so you or an LLM can keep them set up the way you want.
- `prf index` builds the codebase map for a checkout, to preview what Claude is given.
- `prf config` shows the settings in effect; `prf config init` sets up `~/.prf`.

A pipeline can also stop for you: a `@review` step opens a pull-request-style review of the
branch in your browser, and Claude addresses your comments, round after round, until you approve.
See [human review](#human-review). And a role can stop to ask you something: an `@ask` step after
it shows you its question beside the diff, records your answers, and runs the role again. See
[questions from a step](#questions-from-a-step).

A pipeline can also check the work for real instead of taking a role's word for it: `@check` runs
a command, like the tests, and when it fails, a fix role gets the output and fixes the branch, and
the gate runs it again. See [checks](#checks).

Claude never pushes or opens PRs: the sandbox denies it `git push` and `gh`. Each role commits its
work on the branch. With `--push` (and `--pr-create`), prf itself pushes the branch and opens a
PR once every step has passed; see [pushing and opening PRs](#pushing-and-opening-prs).

## Contents

- [Requirements](#requirements)
- [Install](#install)
- [Quick start](#quick-start)
- [How a job runs](#how-a-job-runs)
- [The prf home](#the-prf-home): [conf.json](#confjson), [pipeline.json](#pipelinejson)
- [Roles and pipelines](#roles-and-pipelines): [what a step must leave behind](#what-a-step-must-leave-behind), [writing a role](#writing-a-role), [managing them](#managing-roles-and-pipelines)
- [Human review](#human-review), [questions from a step](#questions-from-a-step), [checks](#checks)
- [The job's environment](#the-jobs-environment): per-job tool caches
- [Codebase map](#codebase-map)
- [Watching jobs](#watching-jobs): [progress output](#progress-output), [prf status](#prf-status), [prf costs](#prf-costs), [status lines](#status-lines), [exit codes](#exit-codes)
- [Sandbox](#sandbox) and [mise](#mise)
- [Command reference](#command-reference)
- [Environment variables](#environment-variables)
- [Driving prf from an orchestrator](#driving-prf-from-an-orchestrator)
- [Pushing and opening PRs](#pushing-and-opening-prs) and [cost caps](#cost-caps)
- [Re-running, resuming and cleaning up](#re-running-resuming-and-cleaning-up)
- [Troubleshooting](#troubleshooting)
- [Development](#development)

## Requirements

- **Claude Code** (`claude`) on your PATH and logged in. prf runs it as `claude -p`.
- **git**.
- **Go** to build prf. The repo pins its version in `mise.toml`.
- **For the sandbox:** macOS works out of the box. Linux and WSL2 need `bubblewrap` and `socat`
  (see [Claude Code's sandboxing docs](https://code.claude.com/docs/en/sandboxing)).
- **For [`@check`](#checks) in a sandboxed job:** `srt`, Anthropic's sandbox runtime:
  `npm install -g @anthropic-ai/sandbox-runtime`.
- **Optional:** mise, if your repos use it for tool versions.

## Install

```sh
git clone git@github.com:chrisrob11/prfoundry.git && cd prfoundry
mise install                          # the Go version this repo pins
go build -o ~/.local/bin/prf .     # any folder on your PATH works
prf config init                    # set up ~/.prf
```

Build to a path of your choosing rather than using `go install`. `go install` names the binary
`prfoundry`, and under mise it puts it in a folder tied to one Go version, which disappears when
you upgrade Go.

To upgrade, pull and run the `go build` line again. `prf config init` is safe to re-run: it
only writes files that don't exist yet.

## Quick start

```sh
# 1. Set up the home folder: conf.json, the built-in roles, and the sandbox settings.
prf config init

# 2. Add a repo alias to ~/.prf/conf.json:
#      "repos": {"app": "git@github.com:me/app.git"}

# 3. Write a PR spec: a markdown file describing the change, e.g. ~/plans/login.md.

# 4. Run it: clone, branch prf/login, then implement -> test -> review.
#    Progress prints as it goes; add -v to see every step Claude takes.
prf run --repo app --branch prf/login --pr ~/plans/login.md

# 5. From another terminal, or afterwards: where is every job?
prf status

# 6. Look at the work.
git -C ~/prf-work/app/login/repo log --oneline
```

Set `pr_dir` and `branch_prefix` in `conf.json` and step 4 becomes `prf run --repo app --pr login`.

To give a repo its own path through the roles:

```sh
prf role set perf-test --repo app < perf-test.md       # a role only this repo uses
prf pipeline set feature --repo app --roles implement:opus,perf-test,test,review:haiku
prf run --repo app --pr login                         # feature is now app's default
```

## How a job runs

For `prf run --repo app --pr login`:

1. **Check everything first.** The PR file is found, the pipeline is picked, and every role is
   loaded and checked. A usage error stops here (exit 2), before anything is cloned.
2. **Clone and branch.** If `conf.json` sets `max_disk_percent` and the disk holding the workdir
   is at least that full, prf stops here (exit 3), before making the job's folder. Otherwise
   the repo is cloned into `<workdir>/app/login/repo` (from `--base` or the default branch). If
   the alias has a `path` to a local checkout, git copies objects from it (`--reference
   --dissociate`) instead of downloading them, so even a big repo clones quickly and the clone
   doesn't depend on the checkout afterwards. If the branch already exists on the remote, prf
   switches to it; otherwise it creates it.
3. **Install tools.** If the alias has [`job_env` or `job_seed`](#the-jobs-environment),
   prf writes `<workdir>/app/login/mise.toml` and makes the copies. Then, if the repo has a mise config, `mise install` runs (see [mise](#mise)).
4. **Prepare context.** The [codebase map](#codebase-map) is built and the repo's `setup` commands
   run, filling `<workdir>/app/login/context/`.
5. **Run each role in order.** Each role is a fresh `claude -p` call in the clone, on the model
   its step names (see [which model each step uses](#roles-and-pipelines)). The run stops at
   the first role that fails: one that exits non-zero, or whose result Claude marks as an error,
   or that doesn't leave the checkout as its [`expect`](#what-a-step-must-leave-behind) says.
   A `@review` step waits for your [review](#human-review) instead, until you approve. An `@ask`
   step shows you the [question](#questions-from-a-step) the role before it left, if it left one.
6. **Ship, if asked.** With `--push`, once every step has passed, prf pushes the branch and,
   with `--pr-create`, opens the PR (see [pushing and opening PRs](#pushing-and-opening-prs)).
7. **Report.** A `PRF_DONE` line goes to the status file and stderr, `state.json` says done or
   failed, and prf exits with the last role's exit code.

Every step is reported as it happens; see [watching jobs](#watching-jobs).

`prf apply` does the same in an existing checkout, skipping the clone (step 2), except that
`--branch` switches to or creates that branch first.

### What Claude is given

Each role runs as:

```
claude -p <prompt> --permission-mode <mode> --output-format stream-json --verbose [--model <model>]
       [--add-dir <PR spec's folder>] [--add-dir <job>/context] [--settings <claude-settings.json>]
       <claude_args from conf.json> <anything after -- on the prf command line>
```

in the clone's directory, with stdin from `--in` if given. prf reads Claude's stream of events to
report progress, so don't override `--output-format` in `claude_args`. The prompt is built from
these parts:

1. The role's text, rendered as a template (see [writing a role](#writing-a-role)). With no role
   (`apply --prompt` only), this is the `--prompt` text itself.
2. A `## Codebase context` section listing what's in `<job>/context/`, asking Claude to read
   `index.md` first and open the `packages/` files it needs.
3. A `## Task` section with the `--prompt` text, when a role is used and `--prompt` was given.
4. A `## This step ran before` section, when the role's step already ran in this job (it failed
   and you [resumed](#re-running-resuming-and-cleaning-up) the job, say): "This step ran before,
   ending at `<sha>`: read `git diff <sha>..HEAD` and its earlier report first, and read
   unchanged code only where you need it." The report is that run's final message, kept in
   `$PRF_TMP/reports/step-N.md`. So a re-run picks up from what it did instead of starting
   over. Gates' fix roles don't get it.

## The prf home

Everything prf reads lives in one folder, `~/.prf` (or `--home`, or `$PRF_HOME`). It's a
good candidate for its own git repo, so your roles and pipelines are versioned and shareable.
Claude never works inside it, so a run can't change its own instructions.

```
~/.prf/
  conf.json                  global settings and repo aliases
  claude-settings.json       sandbox settings for every job
  roles/                     shared roles: implement.md, test.md, review.md, ...
  <repo>/
    pipeline.json            this repo's pipelines, codebase map and setup commands
    roles/                   this repo's roles; override shared roles of the same name
    claude-settings.json     optional: sandbox settings for this repo only
```

**Repo names.** `<repo>` is the name prf knows a repo by. It's the alias you pass to `--repo`
if it's in `conf.json`. Otherwise, for `run` it's the clone URL's last part without `.git`
(`git@github.com:me/app.git` is `app`), and for `apply` it's `--repo` if given, else the
checkout's folder name. Names may only use letters, digits, `-`, `_` and `.`; add an alias for a
repo whose name doesn't fit.

### conf.json

```json
{
  "workdir": "~/prf-work",
  "pr_dir": "~/plans",
  "branch_prefix": "prf/",
  "claude_args": ["--model", "sonnet"],
  "repos": {
    "app": "git@github.com:me/app.git",
    "api": {"url": "git@github.com:me/api.git", "path": "~/code/api", "base": "main", "pr_dir": "~/code/api/plans"}
  }
}
```

| Key | Meaning |
|---|---|
| `workdir` | Where job folders and clones are created (default `~/prf-work`) |
| `pr_dir` | Where a bare `--pr` name is looked up: `--pr login` finds `<pr_dir>/login.md` |
| `branch_prefix` | `run` with `--pr` and no `--branch` uses `<branch_prefix><pr name>`, e.g. `prf/login` |
| `mode` | Claude permission mode (default `acceptEdits`) |
| `model` | Model for steps that don't name one, in any repo without its own `model` (default: Claude's own) |
| `claude` | Claude binary (default `claude`) |
| `claude_args` | Extra arguments for every Claude call, placed before anything after `--`. Use `model` rather than `--model` here, or it overrides every step's model |
| `max_cost`, `max_step_cost` | Default [cost caps](#cost-caps) in USD for the whole job and for each step |
| `sandbox` | `false` turns the sandbox off (default on) |
| `review_open` | `false` stops a [`@review`](#human-review) step from opening its page in your browser; you still get a notification (default `true`) |
| `max_disk_percent` | `run` refuses to clone (exit 3) when the disk holding the workdir is at least this full, in percent used as `df` counts it, e.g. `90`. Default `0`: no check |
| `repos` | Aliases: a URL string, or an object (below) |

A repo alias object can have:

| Key | Meaning |
|---|---|
| `url` | What `run` clones |
| `path` | Your local checkout: what `apply` works in, and where `run` copies git objects from |
| `base` | The branch to clone and branch from, and the PR's base |
| `pr_dir` | Overrides the global `pr_dir` |
| `push` | `true` pushes the branch once every step passes |
| `pr_create` | `"draft"` or `"ready"`: open a PR after pushing (implies `push`) |
| `pr_title_file`, `pr_body_file` | Files in the checkout (e.g. written by a role) with the PR's title and body. Without them, gh fills both from the commits |
| `job_env` | Environment variables for the job, as `{"NAME": "value"}`, written to `<job>/mise.toml` (see [the job's environment](#the-jobs-environment)). `{{job}}` in a value is the job folder |
| `job_seed` | Copies made into a fresh job folder before the first step, as `[{"from": "~/shared", "to": "{{job}}/copy"}]`. `to` must be under `{{job}}` |

`~` is expanded in paths. Unknown keys are an error, so a typo can't silently do nothing.
A non-empty flag beats an env var, which beats `conf.json`, which beats the built-in default.

### pipeline.json

`~/.prf/<repo>/pipeline.json` says which roles a branch of that repo goes through, which model
each step uses, and how to prepare a fresh checkout:

```json
{
  "default": "feature",
  "model": "sonnet",
  "expect": "commits",
  "pipelines": {
    "feature": [
      {"role": "implement", "model": "opus"},
      "perf-test",
      {"role": "layer-check", "expect": "read-only"},
      "test",
      {"role": "review", "model": "haiku"}
    ],
    "bugfix": ["implement", "test"]
  },
  "index": true,
  "setup": ["go list -json ./... > $PRF_CONTEXT/packages.json"],
  "check": "go vet ./... && go test -race ./..."
}
```

| Key | Meaning |
|---|---|
| `pipelines` | Named lists of steps, run in order. A step is a role name, or `{"role": ..., "model": ..., "expect": ...}` to pick its model or what it must leave behind, or a [`@review`](#human-review), [`@ask`](#questions-from-a-step) or [`@check`](#checks) gate |
| `default` | The pipeline that runs when `--pipeline` isn't given |
| `model` | Model for this repo's steps that don't name one (overrides `conf.json`'s `model`) |
| `expect` | What this repo's role steps must leave behind, unless a step object sets its own `expect`: `"commits"`, `"read-only"` or `"clean"` (see [what a step must leave behind](#what-a-step-must-leave-behind)). Default: no check |
| `max_cost`, `max_step_cost` | This repo's [cost caps](#cost-caps), overriding `conf.json`'s. A step object can set its own `max_cost` too |
| `index` | `false` skips the built-in [codebase map](#codebase-map) (default `true`) |
| `setup` | Shell commands run in the checkout before the first role (see [codebase map](#codebase-map)) |
| `check` | The command a [`@check`](#checks) gate runs when it doesn't name its own |

A repo without a `pipeline.json` has one built-in pipeline, `default`: `implement -> test -> review`.
Unknown keys are an error. You can edit the file by hand or use `prf pipeline`, which validates
before saving.

## Roles and pipelines

A **role** is one step: a `<name>.md` prompt that Claude runs as a fresh session. A **pipeline**
is the path a branch takes: an ordered list of roles.

**Which roles run:**

1. `--role a,b`, if given;
2. otherwise `--pipeline <name>` from the repo's `pipeline.json`;
3. otherwise the repo's default pipeline.

`apply` stops after step 2: without `--role` or `--pipeline` it sends `--prompt` to Claude on its
own, with no role. `--role` and `--pipeline` can't be combined.

**Which model each step uses:**

1. `--model`, if given: it overrides every step, e.g. to try a whole pipeline on a cheap model;
2. otherwise the step's own model (`{"role": "implement", "model": "opus"}`, or `implement:opus`
   on the command line);
3. otherwise the repo's `model` in `pipeline.json`;
4. otherwise `model` in `conf.json`;
5. otherwise Claude Code's own default.

A model is anything `claude --model` accepts: an alias like `opus`, `sonnet` or `haiku`, or a
full name like `claude-opus-5-5`. The model shows in progress lines (`[1/5] implement (opus):
started`), status lines (`model=opus`), `prf status`, and `state.json`'s `models`. A `--model`
in `claude_args` or after `--` beats all of these.

**Where roles come from:** the repo's `~/.prf/<repo>/roles/` first, then the shared
`~/.prf/roles/`. So a repo can override `review` without affecting others.

Roles run one after another in the same checkout. Each commits its own work, so the branch history
reads implement, then test, then review. The run stops at the first role that fails.

### What a step must leave behind

A role that says it committed may not have, and a role meant only to look may change things. A
step's `expect` says what the checkout must look like when the role finishes:

| `expect` | The role must |
|---|---|
| `"commits"` | Add at least one commit to the branch, and leave no uncommitted changes |
| `"read-only"` | Add no commits, and leave no uncommitted changes |
| `"clean"` | Leave no uncommitted changes; commits or not |

Uncommitted changes are anything `git status --porcelain` lists, untracked files included. Set it
on a step (`{"role": "implement", "expect": "commits"}`) or for every role step of the repo
(`"expect": "commits"` in `pipeline.json`, which also covers `--role` steps); a step's own wins.
Without either, nothing is checked. Gates don't take it: their fix roles commit what they fix.

A role that breaks its expectation fails the job at that step with exit 10, and the log says what
it found: `[1/3] implement: failed after 4m2s: expected commits, but it made no commits`, or the
first few uncommitted files. `prf resume` runs the step again. A role that stops to ask the
[`@ask`](#questions-from-a-step) gate after it a question isn't checked; its re-run, once
answered, is. In `prf apply`, a checkout with uncommitted changes before the job starts breaks
every expectation.

| Built-in role | What it does |
|---|---|
| `implement` | Implements the PR spec and/or prompt, runs the build and tests, and commits. Needs `--pr` or `--prompt` |
| `test` | Test Engineer: covers every promised behavior with table-driven tests, runs `go test -race ./...`, fixes real bugs as separate `fix:` commits, never weakens a test |
| `review` | Idiomatic Go Reviewer: hunts AI slop (restating comments, needless interfaces, impossible nil checks, sloppy errors, vague names, dead code) and fixes it without changing behavior |
| `answer-question` | Question Recorder: the [`@ask`](#questions-from-a-step) gate's fix role. Records your answers in the PR spec, if there is one, and deletes the question file |
| `address-review` | Review Responder: the [`@review`](#human-review) gate's fix role. Makes the changes your review asks for (or says why not), commits them as `review: ...`, and replies to each comment |
| `fix-check` | Check Fixer: the [`@check`](#checks) gate's fix role. Finds the cause of the failure, fixes it, runs the command again, and commits as `fix: ...`; never weakens a test to pass |

`prf config init` installs these into `~/.prf/roles/`. They're ordinary files, so edit them
freely; a second `init` won't overwrite them, and `prf role reset <name>` restores the original.

### Writing a role

A role is a Go [text/template](https://pkg.go.dev/text/template) that can use:

| Field | Value |
|---|---|
| `{{.PR}}` | Absolute path to the PR spec, or empty |
| `{{.Branch}}` | The branch being worked on |
| `{{.Task}}` | The `--prompt` text, or empty. It's also appended as `## Task`, so roles rarely need it |
| `{{.Review}}` | Only in a [`@review`](#human-review) gate's fix role: your review's comments, as markdown. In an [`@ask`](#questions-from-a-step) gate's: your answers. Empty otherwise |
| `{{.Question}}` | Only in an [`@ask`](#questions-from-a-step) gate's fix role: the question you answered. Empty otherwise |
| `{{.PriorRun}}` | Only when the step ran before in this job: the commit its last run ended at. Empty otherwise |
| `{{.PriorReport}}` | With `{{.PriorRun}}`: the file holding that run's final message, if it left one |

The first line is used as the role's description in `prf role list`, so start with a heading.

```markdown
### Layer Validator

You are checking architectural layering on branch {{.Branch}}.
{{if .PR}}The change is described in {{.PR}}.{{end}}

The rules: `internal/domain` imports nothing from the module; `internal/store` may import only
`internal/domain`; `cmd/` may import anything. The codebase map lists each package's internal
imports. Fix any violation with the smallest change, run `go build ./...` and the tests, and
commit as `refactor: fix layering`. Do not push.

When you finish, list each violation you found and what you changed.
```

Tips:
- Say what to commit and how to name the commit. Every built-in role says "Do not push".
- Say how to scope the work: the PR spec, the task, or "everything changed on this branch relative
  to its base" (see `git log`/`git diff`).
- Ask for a summary at the end. It lands in `claude.log`.
- A role must render a non-empty prompt with or without a PR and a task. `prf role set` checks
  this for you.

### Managing roles and pipelines

Both commands are non-interactive and have JSON output, so an LLM can inspect and change them.

```sh
prf role list [--repo app] [--json]          # what that repo would use: scope (repo/shared), path, description, overrides, valid/error
prf role show <name> [--repo app] [--json]   # the role's text on stdout (--json adds metadata)
prf role set <name> [--repo app] [--file f]  # from --file or stdin; --repo writes the repo's own role, otherwise a shared one
prf role remove <name> [--repo app]          # deletes the repo's role (with --repo) or the shared one
prf role reset <name>                        # restores a built-in's original text in the shared roles

prf pipeline list --repo app [--json]        # the repo's pipelines, the default (*), and valid/error
prf pipeline show <name> --repo app [--json] # its roles, one per line
prf pipeline set <name> --repo app [--roles a,b:opus,c | --file f] [--default]   # steps (role or role:model) on stdin, one per line or comma-separated; # comments
prf pipeline remove <name> --repo app
prf pipeline default <name> --repo app       # make it the one that runs without --pipeline
```

Nothing invalid is saved:
- **Roles:** a role must parse and render a non-empty prompt with every mix of PR file and task.
- **Pipelines:** a pipeline must list at least one role, and every role must exist and be valid
  for that repo.

A rejected `set` exits 2 with the reason and leaves the file unchanged; writes are atomic.
The first pipeline you `set` for a repo becomes its default (and `pipeline.json` is created).
`list` flags anything that has become invalid, such as a pipeline whose role was removed.

The usual edit loop for a role is `prf role show review > r.md`, edit `r.md`, then
`prf role set review --file r.md`.

The JSON forms, for scripts and LLMs. `prf role list --json` (with `--repo`, `scope` can also
be `repo`, and `overrides` gives the path of the shared role it hides):

```json
[
  {"name": "implement", "scope": "shared", "path": "/Users/me/.prf/roles/implement.md",
   "description": "Implementer", "builtin": true, "valid": true},
  {"name": "broken", "scope": "shared", "path": "/Users/me/.prf/roles/broken.md",
   "description": "...", "builtin": false, "valid": false, "error": "role broken: ... can't evaluate field Nope"}
]
```

`prf role show <name> --json` adds `"content"`, the role's text. `prf pipeline list --repo app --json`
(and `pipeline show <name> --json`, for one):

```json
[
  {"name": "feature", "steps": [{"role": "implement", "model": "opus"}, "test"], "default": true, "valid": true},
  {"name": "perf", "steps": ["implement", "perf-test"], "default": false, "valid": false, "error": "no role \"perf-test\" in ..."}
]
```

## Human review

Put `@review` in a pipeline and the job stops there for you: prf serves a review of the branch,
like a pull request's, on `localhost`, opens it in your browser, and notifies you. Nothing to run.

```json
"feature": ["implement", "test", "@review"]
```

On the page you see the branch's diff against its base, its commits, and the PR spec or task.
Click `+` beside a line to comment on it, or comment on a whole file, then finish the review:

- **Request changes:** the gate runs its fix role (`address-review`) with your comments. Claude
  makes the changes, commits them, and replies to each comment. Then the page refreshes for the
  next round, opening on **Since last review**: only what changed since you last looked, with your
  earlier comments and Claude's replies above it.
- **Approve:** the gate passes, and the pipeline carries on (to the next step, or to pushing and
  opening the PR).

The job blocks while it waits, so `prf run` still returns only when the job is done.

Each job has its own review page, so a wave of PRs gives you one link per PR:
`http://127.0.0.1:<port>/<repo>/<job>/<token>/`. The link stays the same for every round and when
the job is [resumed](#re-running-resuming-and-cleaning-up): its port and token are saved in
`<job>/reviews/link.json`. Only if that port has been taken meanwhile does the page move to a
new one, and prf says so. `prf status` shows a waiting job as `review`, with its link, and
the `PRF_REVIEW` status line has it too. If you close the tab, open the link again; pending
comments are kept in the browser. The page is only reachable from your machine, and the random
token keeps other sites from posting a review.

The gate's options, in `pipeline.json`:

```json
{"gate": "review", "fix_role": "address-review", "max_rounds": 5, "model": "opus", "max_cost": 3}
```

| Key | Meaning |
|---|---|
| `fix_role` | The role that addresses your comments (default `address-review`). It gets them in `{{.Review}}` |
| `max_rounds` | How many rounds of requested changes the gate runs before stopping the job with exit 6 (default 5) |
| `model`, `max_cost` | The fix role's model and its budget for each round, as for any step |

On the command line it's `@review` or `@review:model`, e.g. `--role implement:opus,@review:sonnet`.
`review_open: false` in `conf.json` (or `--review-open=false`) keeps the browser closed and only
notifies you.

Every round is saved in `<job>/reviews/`: `round-N.json` (your verdict, comments, the commit you
reviewed, Claude's replies and summary) and `round-N.md` (the review as the fix role saw it). A
gate that's [resumed](#re-running-resuming-and-cleaning-up) carries on from these: if the job
stopped before Claude addressed a round, it does that first, then waits for your next review.

## Questions from a step

Sometimes a role hits a question only you can answer: a design decision the brief didn't settle,
or a change outside its scope. Put `@ask` right after it, and the role can stop and ask:

```json
"feature": ["implement", "@ask", "test", "@review"]
```

The role writes its question, as markdown, to `$PRF_TMP/stop.md` and finishes. (Tell it to in
the role's text or the brief.) `PRF_TMP` is the job's own [tmp folder](#the-jobs-tmp-folder). Then the `@ask` gate:

1. **Passes at once if there's no question file.** So you can put `@ask` after every step that
   might stop. No page opens.
2. **Serves the question on the [review page](#human-review)**, above the branch's diff, and opens
   it in your browser, as `@review` does. `file:line` references in the question (`run.go:120`,
   `scripts/x.sh:4-9`) are links: to that line in the diff, or to the file at HEAD if the line
   isn't in the diff.
3. **Waits for your answers.** Click `+` beside a paragraph, heading or list item of the question to
   answer that part, comment on lines of the diff, and write a general answer. Then:
   - **Answer** sends what you wrote.
   - **Accept recommendations** sends "The owner accepts the option marked Recommended for every
     question", plus anything you wrote.
   - **Stop job** ends the job with exit 8. No further steps run.
4. **Runs its fix role** (`answer-question`) with the question in `{{.Question}}` and your answers in
   `{{.Review}}`. The built-in one records them in the PR spec, if there is one, and deletes the
   question file. A repo can use its own role, to record them wherever it keeps decisions.
5. **Runs the role that asked again.** Its prompt gets every question it asked and your answers,
   under `## Your questions, answered`. If it asks again, the gate asks you again, up to
   `max_rounds` questions. Then the job stops with exit 7.

The gate's options, in `pipeline.json`:

```json
{"gate": "ask", "question": ".agent/tmp/stop.md", "fix_role": "answer-question", "max_rounds": 5, "model": "opus", "max_cost": 1}
```

| Key | Meaning |
|---|---|
| `question` | The question file (default `$PRF_TMP/stop.md`). Environment variables expand as the job's steps see them; a relative path is in the checkout |
| `fix_role` | The role that records your answers (default `answer-question`) |
| `max_rounds` | How many questions the gate takes before stopping the job with exit 7 (default 5) |
| `model`, `max_cost` | The fix role's model and its budget for each round. The role that asked runs again on its own model |

On the command line it's `@ask` or `@ask:model`. Every job has its own `PRF_TMP`, so jobs
running side by side never see each other's questions. Don't use `$TMPDIR` in `question`: inside
the sandbox, Claude Code sets `TMPDIR` to a folder of its own, shared by every job.

`prf status` shows a waiting job as `ask`, with STAGE `ask (waiting for owner)` and the page in
DETAIL. The status lines are `PRF_ASK` and `PRF_ANSWERED`. The question isn't committed: it
stays out of the branch's history. Each question and your answers are saved in
`<job>/asks/step-N/`: `round-M.json` and `round-M.md` (your answers as the fix role saw them). A
[resumed](#re-running-resuming-and-cleaning-up) gate carries on from these: it records answers the
job stopped before recording, re-runs the role if it hadn't yet, and asks again if the question
file is still there (after **Stop job**, for example).

## Checks

A role saying "the tests pass" isn't the same as the tests passing. `@check` checks for real, and
loops until the check passes:

```json
"feature": ["implement", "test", "@check", "review"]
```

`@check` runs a shell command in the checkout and passes when it exits 0. When it fails, its fix
role (`fix-check`) gets the command and what failed in `{{.Review}}`, fixes the cause, and
commits. Then the gate runs the command again. After
`max_rounds` fixes that still fail, the job stops with exit 9.

The command is the step's `run`, or else the repo's `check` in `pipeline.json`. So with
`"check": "go test ./..."` set, `@check` works on the command line too: `--role implement,@check`.

```json
{"gate": "check", "run": "go vet ./... && go test -race ./...", "timeout": "15m", "fix_role": "fix-check", "max_rounds": 3, "model": "sonnet"}
```

| Key | Meaning |
|---|---|
| `run` | The command, run with `sh -c` in the checkout (default: the repo's `check`; one of the two is required) |
| `timeout` | Stop the command, and everything it started, after this long (`90s`, `15m`, `1h`), and count it as failed. Default: no limit |
| `fix_role` | The role that fixes what failed (default `fix-check`). It gets the failure in `{{.Review}}` |
| `max_rounds` | How many fixes the gate runs before stopping the job with exit 9 (default 5) |
| `model`, `max_cost` | The fix role's model and its budget for each round, as for any step |

**What the fix role reads.** Not the whole output, which can be thousands of lines, nor just its
end, which can miss a failure printed early. When the output is Go's, prf picks out what failed,
wherever it is: build, vet and lint errors (`file.go:12:5: ...`, each once), each failed test with
its log (with or without `-v`), panics with the failing goroutine's stack, data race reports, and
the `FAIL` package lines. Passing tests and packages are left out. Each failure is capped at 60
lines, with 20 failures and 400 lines in all, keeping the first ones. Any lines at the end of the
output that the excerpt doesn't already show (a `make` error, say) are added after it. For output
that isn't Go's, it gets the last 200 lines. Either way, the prompt says where the full output is,
in the job's [tmp folder](#the-jobs-tmp-folder), for the role to read if it needs more.

**It runs in Claude's sandbox.** The command runs the code the steps wrote, so when the job is
sandboxed it runs in the same sandbox as Claude, with the same limits from
`claude-settings.json`: the same network allowlist (and local binding and Unix sockets, so a
test that starts a server on localhost passes here when it passes in Claude's shell), the same unreadable folders (including
`Read(...)` and `Edit(...)` rules in its `permissions.deny`), and writes only to the checkout, the
job's tmp folder and the tool caches. The checkout's `.claude/` and `.mcp.json` stay unwritable,
so the command can't plant settings or a hook that the next Claude step would load. prf uses
[`srt`](https://github.com/anthropics/sandbox-runtime), Anthropic's sandbox runtime and
the one Claude Code itself uses, to do it. Install it with `npm install -g
@anthropic-ai/sandbox-runtime`. If it isn't installed, `@check` stops the job (exit 3) rather than
run the command unsandboxed. A job run with `--sandbox=false`, or whose Claude settings turn the
sandbox off, runs the command directly, as you.

**How it runs.** The command runs with `sh -c`, sandboxed or not, with a short `TMPDIR` of its
own (in your temp folder, removed when the gate ends), as it would have if you ran it, and
`PRF_TMP` set to the job's tmp folder. It gets a process group of its own. When it times out, or
the job is stopped (Ctrl-C, `kill`, or the terminal closing), the whole group gets `SIGTERM`, then
`SIGKILL` 5 seconds later, so tests can clean up. When the command ends, anything it left running
in the background is killed. Its exit code decides, even if a leftover process kept its output
open. A command killed by a signal shows as exit 128 plus the signal.

The command's output goes to `claude.log` as it runs, and each run's output is saved in
`<job>/checks/step-N/round-M.log`, with the failure as the fix role saw it in `round-M.md`. A
[resumed](#re-running-resuming-and-cleaning-up) gate runs the command again, numbering its rounds
after the saved ones, with a fresh set of fixes. It uses the step's `run` as the job was started
with, or the repo's `check` as `pipeline.json` says now.

## The job's tmp folder

Every job has its own scratch folder, `<job>/tmp`. Steps get it as `PRF_TMP`, and Claude may
write there, inside the sandbox too. Use `PRF_TMP` in roles for files that should outlive a
step, like an `@ask` question: inside the sandbox, Claude Code points `TMPDIR` at a folder of its
own, which every job shares. prf doesn't change `TMPDIR`: a job's folder can be too deep for a
test's unix socket. The folder lives as long as the job, through resumes, and `prf clean`
removes it with the job.

## The job's environment

A repo alias's `job_env` gives every job of that repo its own environment, such as its own tool
caches inside the job folder, so `prf clean` frees them with the job and jobs side by side
don't share them. For a Go repo:

```json
"server": {
  "url": "git@github.com:me/server.git",
  "job_env": {"GOCACHE": "{{job}}/cache/go-build", "GOLANGCI_LINT_CACHE": "{{job}}/cache/golangci"},
  "job_seed": [{"from": "~/Library/Caches/go-build", "to": "{{job}}/cache/go-build"}]
}
```

When `run` makes the job folder, it writes `<job>/mise.toml` with these in its `[env]` table,
`{{job}}` replaced by the job folder's path. The clone is `<job>/repo`, so mise picks the file up
from anywhere in the clone, together with the repo's own mise config: the setup commands, every
step, and you, if you `cd` into the clone and run `make` or `go`, all get the same environment,
even with those variables exported to something else. mise's own templates, like
`{{config_root}}`, pass through. Without `job_env`, no `mise.toml` is written. It needs mise to
be active in your shell (`mise activate` or its shims) to apply outside prf.

`job_seed` copies each `from` (`~` is expanded) to `to` before the first step, so a cache can start
warm from a shared one. On macOS it uses `cp -c`, which clones the files on APFS instead of copying
them, so even a big cache takes no time or space until it changes; elsewhere it's a plain copy. A
missing `from`, or a `to` that's already there, is a warning, and so is a failed copy, which
leaves nothing half-copied behind. Copy a Go module cache only if it's writable (`GOFLAGS=-modcacherw`),
or `prf clean` can't remove it.

A `job_env` value that's a path under `{{job}}`, and every `to`, is treated as a folder: prf
makes it and lets steps write to it inside the [sandbox](#sandbox), for Claude and for
[`@check`](#checks) commands. Nothing else in the job folder becomes writable. Only `run` writes
`mise.toml` and seeds; `resume`, and `apply` in a `run` clone, keep the job's file and its folders.
`apply` in your own checkout doesn't use them at all.

## Codebase map

Every role is a fresh Claude session, so without help each one would explore the repo from
scratch. Before the first role, prf fills `<job>/context/` once:

1. **The Go codebase map.** It takes well under a second (about 80 ms for a 164-package,
   2,000-file repo), uses no LLM, and covers the Go files git tracks that the go tool would build:
   no tests, nothing under `testdata/`, `vendor/`, or folders starting with `.` or `_`. Nothing in
   it is cut short, however big the repo; it grows by one short entry per package, and the detail
   is split into a file per package so Claude opens only what it needs.
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

   `prf index --dir <checkout> --out <folder>` builds the same map without running anything
   else, so you can see what Claude will be given.
2. **Your setup commands,** from the repo's `pipeline.json` `setup` list. They run with `sh -c` in
   the checkout, in order, after `mise install` and outside the sandbox. They get
   `$PRF_CONTEXT` (the context folder) and `$PRF_JOB` (the job folder). Anything they write
   to `$PRF_CONTEXT` is handed to Claude too. A failing command stops the job at setup
   (exit 3), before Claude runs. Use them for code generation (`make generate`) or other indexes:

   ```json
   "setup": [
     "ctags -R -f $PRF_CONTEXT/tags .",
     "go list -json ./... > $PRF_CONTEXT/packages.json"
   ]
   ```

Every role gets read access to the context folder. Its prompt lists what's at the top of the folder
(`index.md`, `packages/`, and anything setup wrote), not every package file, and says to read
`index.md` first. The map is never written into the repo, so it can't be committed. It's rebuilt
on every run, including `apply`. Set `"index": false` to skip the built-in map, for example when a setup command writes
your own `index.md`.

## Watching jobs

Every job gets its own folder under `workdir`:

```
<workdir>/<repo>/<job>/
  repo/          the clone (run only; apply works in your existing checkout)
  context/       the codebase map (index.md, packages/, symbols.json) and setup output
  claude.log     each role's steps and summary, under "=== prf phase <role> (step 2/5, ...) ==="
  claude.jsonl   Claude's raw event stream, for tools that want every detail
  status         one line per event (see status lines)
  state.json     where the job is now (see prf status)
```

`<job>` is the PR file's name without `.md` if there's a PR, otherwise the branch with `/` turned
into `-`.

### Progress output

While a job runs, prf prints progress to stderr:

```
prf: cloning git@github.com:me/app.git -> /Users/me/prf-work/app/login/repo
prf: creating branch prf/login
prf: building codebase map
prf: [1/3] implement (opus): started
prf: [1/3] implement (opus): working (30s, 6 tool calls; last: [edit] internal/auth/login.go)
prf: [1/3] implement (opus): working (1m0s, 11 tool calls; last: [bash] go test ./internal/auth/...)
prf: [1/3] implement (opus): done in 1m24s (14 turns, $0.38)
prf: [2/3] test: started
...
prf: pipeline default: 3/3 steps succeeded in 6m10s, $1.12
```

A "working" line comes every 30 seconds with Claude's latest action. With `-v` (`--verbose`),
every step Claude takes is printed too, as it happens:

```
  [read] internal/auth/login.go
  [bash] go test ./internal/auth/...
  [edit] internal/auth/login.go
  [claude] The token check now rejects expired tokens; running the full suite next.
```

Those steps always go to `claude.log`, with or without `-v`. When each role finishes, Claude's
summary of what it did is printed to stdout (and kept in `claude.log`). `--out <file>` writes the
step log there instead of `claude.log`, and keeps the summaries off stdout.

### prf status

`prf status` shows every job under the workdir, most recently active first:

```
REPO  JOB     STATE    STEP  STAGE                         TIME     DETAIL
app   login   running  2/3   test                          4m12s    [bash] go test ./...
app   signup  setup    -     setup: building codebase map  3s
app   fix-db  done     3/3   review                        1h ago   exit 0, $0.91
api   perf    failed   2/4   perf-test                     2h ago   exit 1, $0.40
api   old     died     1/3   implement                     1d ago   process 48211 is gone
```

| State | Meaning |
|---|---|
| `setup` | Cloning, installing tools, building the map, or running setup commands (the stage says which) |
| `running` | A role is running; STEP is its place in the pipeline, DETAIL is Claude's latest action |
| `review` | Waiting at a [`@review`](#human-review) gate for you; DETAIL is the review page's URL |
| `ask` | Waiting at an [`@ask`](#questions-from-a-step) gate for your answers; STAGE is `ask (waiting for owner)`, DETAIL the page's URL |
| `paused` | Claude's usage limit stopped a step. The job waits until 5 minutes past the reset the error names (30 minutes if it names none), then runs that step again; DETAIL says when. The status file gets a `PRF_PAUSE ... until=<time>` line. Any other failure still fails the job |
| `done` | Every role succeeded |
| `failed` | A role or setup step failed; STAGE is where it stopped |
| `died` | It says it's running, but its prf process is gone (killed, machine restarted) |

`--repo app` shows one repo's jobs. `--json` prints the full state of each job, the same as its
`state.json`:

```json
{
  "repo": "app", "job": "login", "branch": "prf/login",
  "pipeline": "default", "roles": ["implement", "test", "review"],
  "models": ["opus", "sonnet", "haiku"],
  "state": "running", "stage": "test", "step": 2,
  "activity": "[bash] go test ./...",
  "cost": 0.38, "dir": "/Users/me/prf-work/app/login/repo",
  "job_dir": "/Users/me/prf-work/app/login", "pid": 48211,
  "started": "2026-10-29T14:02:11Z", "updated": "2026-10-29T14:06:23Z",
  "pr": "/Users/me/plans/login.md", "task": "", "push": true, "pr_create": "draft"
}
```

While waiting at a review gate, a job has `review_url`. `steps` is the pipeline with each gate's
settings, and `base_commit` is where the branch's changes start, for the review's diff.
`step_heads` holds, by step number, the commit each role step ended at when it last finished,
successfully or not, for a [re-run](#what-claude-is-given) of that step. Once
finished, a job also has `exit`, and after shipping `pushed: true` and `pr_url`. `pr`, `task`,
`push` and `pr_create` are what `prf resume` needs to carry on the same way. `spend` is what
each role cost, by role and model, summed over every run of it (fix rounds, re-runs and resumes):
`runs`, `turns`, `cost`, and `cache_read_tokens`, the context Claude re-read across those turns.

`state.json` is rewritten at every change, and every 2 seconds while Claude is working, and it's
always replaced in one step, so a reader never sees half a file. `exit` appears once the job has
finished.

### prf costs

`prf costs` sums every job's `spend` by role and model, most expensive first, so you can see
which step of a pipeline the money goes to:

```
ROLE         MODEL   COST    SHARE  RUNS  TURNS/RUN  CTX/TURN
plan-brief   opus    $41.20  38.5%  29    40.6       150k
plan-replan  opus    $24.90  23.3%  30    32.9       99k
review       opus    $13.40  12.5%  39    14.6       59k
total $107.00
```

CTX/TURN is the context Claude re-read per turn, in thousands of tokens. A long step pays for its
whole context again on every turn, so a role with many turns at a big context is the one to trim:
have it read less, or give it a cheaper model. `--since 48h` counts only jobs active in that time,
`--repo` one repo's, and `--json` prints the rows. Jobs that ran before prf recorded spend
count in the total only, and the last line says how much of it that is.

### Status lines

The `status` file gets one line per event, and the same lines go to stderr:

```
PRF_START branch=prf/login phase=implement step=1/3 pipeline=default model=opus
PRF_PHASE branch=prf/login phase=implement step=1/3 exit=0 error=false turns=14 cost=0.38
PRF_START branch=prf/login phase=test step=2/3 pipeline=default
PRF_PHASE branch=prf/login phase=test step=2/3 exit=0 error=false turns=9 cost=0.31
PRF_START branch=prf/login phase=review step=3/3 pipeline=default
PRF_PHASE branch=prf/login phase=review step=3/3 exit=0 error=false turns=12 cost=0.43
PRF_DONE branch=prf/login phase=review exit=0 dir=/Users/me/prf-work/app/login/repo
```

- `PRF_START`: a role started, at this step of this pipeline, on this model (`model=` is left
  out when Claude's own default is used).
- `PRF_PHASE`: a role finished. `exit` is the code prf uses; `error`, `turns` and `cost` come
  from Claude's own result (left out if Claude didn't send one).
- `PRF_PHASE ... stopped=max_cost`: the step hit its budget, or the job's cap ran out before it
  started (see [cost caps](#cost-caps)).
- `PRF_PHASE ... exit=10 ... expect=commits`: the role broke its
  [expectation](#what-a-step-must-leave-behind); the log says how.
- `PRF_REVIEW branch=... phase=@review step=... round=N url=...`: a review gate is waiting for
  your review of round N at `url`. `PRF_REVIEWED ... round=N verdict=approve|changes comments=K`:
  you submitted it. Between rounds, the fix role runs with its own `PRF_START` and
  `PRF_PHASE` lines (`phase=address-review`), at the gate's step. The gate ends with
  `PRF_PHASE ... phase=@review ... exit=... rounds=N`, plus `stopped=max_rounds` if it ran out.
- `PRF_ASK branch=... phase=@ask step=... round=N url=...`: an ask gate is waiting for your
  answers to question N at `url`. `PRF_ANSWERED ... round=N verdict=answer|accept|stop
  comments=K`: you answered. The fix role and the re-run of the role that asked follow, with their
  own `PRF_START` and `PRF_PHASE` lines at the gate's step. The gate ends with
  `PRF_PHASE ... phase=@ask ... exit=... rounds=N`, plus `stopped=max_rounds` or `stopped=owner`.
- `PRF_CHECK branch=... phase=@check step=... round=N exit=...`: a check gate ran its command,
  plus `stopped=timeout` if it ran out of time. Between rounds, the fix role runs with its own
  `PRF_START` and `PRF_PHASE` lines (`phase=fix-check`). The gate ends with
  `PRF_PHASE ... phase=@check ... exit=... rounds=N`, plus `stopped=max_rounds` if it ran out.
- `PRF_PUSH branch=... exit=...` and `PRF_PR branch=... url=...` (or `exit=1`): prf pushed
  and opened the PR, after the last step and before `PRF_DONE`.
- `PRF_DONE`: the job finished. It's always the last line written. `phase` is the last role
  that ran, or `setup` if the job failed before any role. `dir` is where the work is.

The file is appended to, so a [resumed](#re-running-resuming-and-cleaning-up) job adds to it. `--status`
writes these lines to another file instead.

### Exit codes

| Exit code | Meaning |
|---|---|
| `0` | Every role succeeded |
| `1`, or another non-zero code | A role failed: its Claude exit code, or `1` if Claude exited cleanly but marked its result as an error. Later roles didn't run |
| `2` | Usage error: bad flags, unknown role or pipeline, missing PR file, invalid name. Nothing was cloned |
| `3` | Setup failed: the job's clone already exists, the workdir's disk is fuller than `max_disk_percent`, clone or branch failed, a setup command failed, or a file couldn't be written. Also a `@check` command that can't start or be sandboxed (no `srt`) |
| `4` | Every step passed, but pushing or opening the PR failed: the work is done but didn't ship. |
| `5` | A [cost cap](#cost-caps) stopped the job |
| `6` | A [`@review`](#human-review) gate ran `max_rounds` rounds and you still requested changes. `prf resume` carries on |
| `7` | An [`@ask`](#questions-from-a-step) gate took `max_rounds` questions and the role asked again. `prf resume` carries on |
| `8` | You chose **Stop job** at an [`@ask`](#questions-from-a-step) gate. `prf resume` asks the question again |
| `9` | A [`@check`](#checks) gate's command still failed after `max_rounds` fixes. `prf resume` runs it again |
| `10` | A role didn't leave the checkout as its [`expect`](#what-a-step-must-leave-behind) says: no commits, commits it shouldn't have made, or uncommitted changes. `prf resume` runs it again |
| `130` | The job was interrupted (Ctrl-C, `kill`, or its terminal closing), or Claude was killed by a signal |

Exit 0 means Claude finished without reporting an error, not that the work is right. Check the
commits and Claude's summaries. On Ctrl-C, Claude gets the signal and prf still writes
`PRF_DONE`.

## Sandbox

Claude runs inside Claude Code's sandbox by default. prf passes `--settings` with
`~/.prf/<repo>/claude-settings.json` if it exists, otherwise `~/.prf/claude-settings.json`.
It writes the shared one with the defaults below the first time it's needed (or on
`prf config init`), and never rewrites it.

It's an ordinary [Claude Code settings file](https://code.claude.com/docs/en/settings), so edit it
to change what Claude can reach. The defaults:

- **Prompts:** shell commands run without approval prompts.
- **Writes:** only to the job's checkout, its [tmp folder](#the-jobs-tmp-folder), the folders its
  [`job_env` and `job_seed`](#the-jobs-environment) name, the Go caches (`GOMODCACHE`, `GOCACHE`,
  `$GOPATH/pkg/sumdb`, as `go env` reports them when the settings file is first written) and
  mise's state folder.
- **Network:** only GitHub (`github.com`, `*.github.com`, `*.githubusercontent.com`) and the Go
  module proxy and checksum servers (`proxy.golang.org`, `sum.golang.org`,
  `storage.googleapis.com`). Any other host is refused, not prompted for.
- **Credentials:** `~/.ssh`, `~/.aws`, `~/.gnupg`, `~/.config/gh`, `~/.netrc` and `~/.docker`
  can't be read, by shell commands or by Claude's Read tool.
- **No pushing:** `git push` and `gh` are denied, plain and `rtk`-prefixed (the rtk hook rewrites
  commands before the check, so both forms matter). prf pushes itself; see
  [pushing and opening PRs](#pushing-and-opening-prs).
- **No way out:** Claude exits if the sandbox can't start, and can't retry a blocked command
  outside it.
- **Go TLS:** `enableWeakerNetworkIsolation` is on. Without it, Go can't verify TLS certificates
  inside the macOS sandbox, so `go get` fails.

Common changes: add hosts to `sandbox.network.allowedDomains` (a private module proxy, your
artifact registry), or add folders to `sandbox.filesystem.allowWrite`. Put repo-specific changes
in `~/.prf/<repo>/claude-settings.json`, which replaces the shared file for that repo.

The sandbox only limits shell commands. File edits are governed by `--mode`; the default
`acceptEdits` auto-approves edits inside the working directory only. Use `--sandbox=false` for
one run, or `"sandbox": false` in `conf.json`, to turn the sandbox off.

### mise

If the repo has a mise config (`mise.toml`, `.tool-versions`, ...) and mise is installed, prf
runs `mise install` in the checkout before Claude starts. That happens outside the sandbox,
because the sandbox blocks mise from downloading tool versions. A failed install is only a
warning.

For `run`, prf also sets `MISE_TRUSTED_CONFIG_PATHS` to the workdir, for that install, the
setup commands, and Claude. Fresh clones under the workdir, and the `mise.toml` prf writes
beside them from [`job_env`](#the-jobs-environment), are then trusted for these runs only, without
`mise trust` and without changing your mise settings. `resume`, and `apply` in a clone under the
workdir, do the same. `apply` in your own checkout doesn't, so mise's trust stays as you set it.
To use a job's environment from your own shell, run `mise trust <job>/mise.toml` once (and the
clone's own config, if it has one), or add the workdir to mise's `trusted_config_paths`.

## Command reference

`prf [--home <dir>] <command> ...`. The global `--home` (env `PRF_HOME`, default
`~/.prf`) can go before or after the command. Every command has `--help`.

### prf run

Clone a repo into a fresh job folder, switch to a branch, and run a pipeline.

| Flag | Meaning |
|---|---|
| `--repo` | Repo URL, local path, or alias from `conf.json` (required) |
| `--branch` | Branch to create, or switch to if it exists on the remote. Default `<branch_prefix><pr name>`; required without `--pr` |
| `--base` | Branch to clone and branch from (default: the alias's `base`, else the remote's default branch) |

Plus the [shared flags](#shared-flags).

### prf apply

Run a pipeline, a list of roles, or a single prompt in an existing checkout.

| Flag | Meaning |
|---|---|
| `--repo` | The repo's name: picks its pipelines, roles and job folder. An alias's `path` is used as the checkout unless `--dir` is given |
| `--dir` | The checkout to work in (default: the current directory) |
| `--branch` | Switch to this branch first, creating it if needed (default: stay on the current branch) |

Plus the [shared flags](#shared-flags). `apply` warns if the working tree has uncommitted
changes, because Claude's commits may include them.

### Shared flags

| Flag | Meaning |
|---|---|
| `--pr` | PR spec file. Tried as given, then as `<pr_dir>/<name>` and `<pr_dir>/<name>.md` |
| `--prompt` | Task text. Added to every role's prompt; with no role, it is the whole prompt |
| `--pipeline` | Pipeline from the repo's `pipeline.json` (`run` default: the repo's default pipeline; `apply` default: none) |
| `--role` | Roles to run in order instead of a pipeline, comma-separated or repeated. `role:model` picks a step's model, e.g. `--role implement:opus,review:haiku` |
| `--model` | Model for every step, overriding the pipeline's (see [which model each step uses](#roles-and-pipelines)) |
| `--max-cost`, `--max-step-cost` | [Cost caps](#cost-caps) in USD for the job and for every step |
| `--push` | Push the branch once every step passes; `--push=false` skips it once |
| `--pr-create` | After pushing, open a `draft` or `ready` PR; `none` skips it once |
| `--workdir` | Where job folders go (env `PRF_WORKDIR`; default `conf.json`'s `workdir`, else `~/prf-work`) |
| `-v`, `--verbose` | Also print every step Claude takes (files read, commands, edits, messages) as it happens |
| `--out` | Write the step log to this file instead of `<job>/claude.log`, and keep Claude's summaries off stdout |
| `--in` | Extra context on Claude's stdin. A regular file goes to every role; a fifo or `-` (prf's own stdin) can only be read once, so it goes to the first role |
| `--status` | Append status lines to this file instead of `<job>/status` |
| `--mode` | Claude permission mode (default `conf.json`'s `mode`, else `acceptEdits`) |
| `--sandbox` | Run in Claude Code's sandbox (default on; `--sandbox=false` turns it off) |
| `--review-open` | At a [`@review`](#human-review) gate, open the review page in your browser (default `conf.json`'s `review_open`, else on; `--review-open=false` only notifies) |
| `--claude` | Claude binary (env `CLAUDE_BIN`; default `conf.json`'s `claude`, else `claude`) |
| `-- ...` | Everything after `--` is passed to `claude`, e.g. `-- --model opus` |

### prf status

`prf status [--repo <name>] [--json] [--workdir <dir>]`: every job's state, step, stage and
latest activity. See [prf status](#prf-status).

### prf resume

`prf resume <repo>/<job> [--from <step>] [flags] [-- extra claude args]`: re-run a failed or
died job from where it stopped. See [resuming](#re-running-resuming-and-cleaning-up).

### prf clean

`prf clean [--repo <name>] [--failed] [--force] [--yes]`: remove finished job folders; a dry run
unless `--yes`. See [cleaning up](#re-running-resuming-and-cleaning-up).

### prf index

`prf index [--dir <checkout>] [--out <folder>]`: build the [codebase map](#codebase-map) for a
checkout (default: the current directory, into `./codebase-map`), without running Claude.

### prf role, prf pipeline

See [managing roles and pipelines](#managing-roles-and-pipelines).

### prf config

`prf config` prints the home folder, which repos have a `pipeline.json` or `roles/` there, and
the `conf.json` settings in effect, as JSON. `prf config init` writes a starter `conf.json`, the built-in roles
and `claude-settings.json`, skipping any that already exist.

## Environment variables

| Variable | Used for |
|---|---|
| `PRF_HOME` | The prf home (same as `--home`) |
| `PRF_WORKDIR` | Where job folders go (same as `--workdir`) |
| `CLAUDE_BIN` | The Claude binary (same as `--claude`) |

Every step gets `PRF_TMP`, the job's [tmp folder](#the-jobs-tmp-folder).

Setup commands also get:

| Variable | Value |
|---|---|
| `PRF_CONTEXT` | `<job>/context`: write files here to hand them to Claude |
| `PRF_JOB` | The job folder |
| `MISE_TRUSTED_CONFIG_PATHS` | The workdir, prepended to any value you set (`run` only; Claude gets it too) |

## Driving prf from an orchestrator

prf is meant to be called by another program, often a small local LLM. What it needs to know:

- **Start a job:** `prf run --repo <alias> --pr <name>`, one per PR. Run them in the
  background to run a wave in parallel; each has its own clone and branch.
- **Know where every job is:** `prf status --json`. Each entry has `state` (setup, running,
  review, ask, done, failed, died), `step` and `roles` (step 2 of 5), `stage` (the role or setup step), and
  `activity` (Claude's latest action, updated every 2 seconds). For one job, read its
  `state.json` directly.
- **Ship and recover:** with `--push --pr-create draft`, a job that exits 0 has pushed and opened
  its PR (`pr_url` in `state.json`). Exit 4 means the work passed but didn't ship; exit 5 means a
  cost cap stopped it. Use `prf resume <repo>/<job>` for failed or died jobs, and
  `prf clean --yes` to reclaim disk from finished ones.
- **Human review:** a job at a `@review` gate is in state `review`, and one at an `@ask` gate in
  state `ask`; both wait for a person, so don't treat them as stuck. `review_url` in `state.json`
  is the page, and `PRF_REVIEW` or `PRF_ASK` says the same. Exit 8 means a person stopped the
  job at an `@ask` gate, not that it failed.
- **Checks:** exit 9 means the `@check` command still failed after its fixes; worth a look by a
  person before a resume.
- **Know whether it worked:** `state` is `done` and `exit` is 0 when every role finished without
  error. Claude's summary of each role is in `claude.log`, and the commits are in `git log` in
  `dir`. A `died` job's process is gone; [resume](#re-running-resuming-and-cleaning-up) it.
- **Set up roles and pipelines:** `prf role list --json`, `prf pipeline list --repo <r> --json`,
  and `set` with the text on stdin. A rejected `set` explains why on stderr and exits 2. Give a step
  a model with `role:model`, e.g. `prf pipeline set feature --repo <r> --roles implement:opus,test,review:haiku`.
- **Pick models:** set them per step in the pipeline, or pass `--model <model>` to run every step of
  one job on the same model (handy for a cheap trial run).
- **Don't parse anything else.** Log lines starting `prf:` are for people and may change; the
  JSON output, `state.json`, and the `PRF_*` lines are the stable interface.

A wave in shell:

```sh
for pr in ~/plans/wave1/*.md; do
  prf run --repo app --pr "$pr" > /dev/null 2>&1 &
done
wait
prf status --repo app
```

Or watch a running wave: `while sleep 5; do clear; prf status; done`.

Plan waves so PRs in the same wave don't edit the same files, and so a wave only depends on
earlier waves that have been merged (or pass `--base` with the earlier wave's branch).

## Pushing and opening PRs

Claude is never allowed to push: the default sandbox settings deny `git push` and `gh`, plain and
`rtk`-prefixed. Shipping is prf's job, so "only pushed if every step passed" is a guarantee of
the program, not a request in a prompt.

Turn it on per run with `--push` and `--pr-create draft|ready`, or per repo with `"push": true` and
`"pr_create": "draft"` on its alias in `conf.json`. `--push=false` or `--pr-create none` skips it
once. A PR implies a push.

When every step has exited 0, prf, running as you outside the sandbox with your own git and gh
credentials:

1. Runs `git push -u origin <branch>`. Never with `--force`: a rejected push is reported, not
   overridden.
2. With a PR asked for, opens it: `gh pr create --head <branch> [--base <base>] [--draft]`, with
   `--title` and `--body-file` from the alias's `pr_title_file` and `pr_body_file` (paths in the
   checkout, which a role can write), or `--fill` without them. If a PR for the branch is already
   open, as after a resume, prf reports that one instead of opening another.

The results go to the status file (`PRF_PUSH`, `PRF_PR`), to `state.json` (`pushed`,
`pr_url`), and to `prf status`, whose DETAIL column shows the PR URL. If the push or the PR
fails, or the title or body file isn't there, the job exits 4: the work passed but didn't ship. A
failed step never ships.

## Cost caps

Two caps, in USD:

- **Per job:** `--max-cost`, else the repo's `max_cost` in `pipeline.json`, else `conf.json`'s.
  Before each step, if the job has used it up, the job stops.
- **Per step:** `--max-step-cost` for every step, else the step's own `max_cost`
  (`{"role": "implement", "model": "opus", "max_cost": 3}`), else the repo's `max_step_cost`,
  else `conf.json`'s.

Each step runs with Claude's `--max-budget-usd` set to the smaller of its own cap and what's left of
the job's. A step that reaches it stops, the job exits 5, and its status line ends
`stopped=max_cost`. Claude checks the budget between turns, so a step can go a little over.

## Re-running, resuming and cleaning up

- **Resume a failed or died job:** `prf resume <repo>/<job>` (as `prf status` shows it, or a
  job folder's path) re-runs from the step that stopped, in the same checkout, with the same PR
  spec, prompt, models and shipping settings, and the current role files. Setup isn't repeated.
  Steps keep their numbers (step 2/3 stays 2/3), status lines are appended, and the cost so far
  counts toward `--max-cost`. A step that ran before is told where its last run ended and where
  its report is (see [what Claude is given](#what-claude-is-given)). `--from <step>` starts elsewhere, including in a finished job;
  `--model`, `--max-cost`, `--max-step-cost`, `--push`, `--pr-create`, `--review-open` and `-v` work as in `run`.
- **Start over:** `run` refuses a job whose clone already exists (exit 3) rather than reuse it,
  and leaves that job's status and `state.json` untouched. Remove the job (below) or use a
  different `--branch`/`--pr`.
- **Clean up:** `prf clean` lists the job folders it would remove: jobs that are done
  (`--failed` adds failed and died ones), never running ones. It keeps any job whose clone has
  uncommitted changes or commits on no remote, saying why, unless you add `--force`. Add `--yes`
  to remove them; it reports the space freed. `--repo` limits it to one repo.
- **Try a role on your own checkout:** `prf apply --repo app --role review` (with the alias's
  `path`), or `prf apply --dir ~/code/app --role review`.

## Troubleshooting

| Problem | Fix |
|---|---|
| `no role "implement" in ...` | Run `prf config init` to install the built-in roles |
| `can't use "x" as a repo name` | The URL or folder name has characters prf can't use as a folder name. Add an alias in `conf.json` |
| `destination already exists` (exit 3) | That job ran before. See [re-running](#re-running-resuming-and-cleaning-up) |
| `the disk holding ... is 93% full` (exit 3) | The workdir's disk is past `max_disk_percent`. `prf clean` frees finished jobs' space |
| A command fails with `Operation not permitted` in `claude.log` | The sandbox blocked a write. Add the folder to `sandbox.filesystem.allowWrite` in `claude-settings.json` |
| `CONNECT tunnel failed, response 403` or "host is not on the allow list" | The sandbox blocked a host. Add it to `sandbox.network.allowedDomains` |
| `x509: OSStatus -26276` from Go | Go's TLS check in the macOS sandbox. Make sure `enableWeakerNetworkIsolation` is `true` |
| mise says a config is "not trusted" | For `run` this is handled; for `apply`, run `mise trust` in your checkout |
| `claude.log` says a command was denied or needed approval | Headless Claude can't ask, so anything not auto-approved is refused. That happens to shell commands when the sandbox is off, and to edits outside the checkout. Keep the sandbox on, or use `--mode bypassPermissions` in a throwaway clone |
| Sandbox fixes in a new prf version don't show up (e.g. Claude can still `git push`) | prf never rewrites `claude-settings.json`. Delete it and the next run (or `config init`) writes the new defaults; copy back anything you'd added |
| `unknown field` in `conf.json` or `pipeline.json` | A typo in a key. The message names it |

## Development

```sh
go test -race ./...
```

The tests run the whole CLI in-process against throwaway git repos and a fake `claude` script that
speaks stream-json, so they need `git` but not Claude, and cost no tokens. To time the codebase
map on a real repo: `PRF_BENCH_DIR=~/code/big go test -run x -bench BuildIndex`.

| File | What's in it |
|---|---|
| `main.go` | Commands, flags, and how `run` and `apply` assemble a job |
| `run.go` | Running the roles, progress output, status lines, git helpers |
| `stream.go` | Reading Claude's stream-json events into the step feed and result |
| `state.go` | `state.json` and `prf status` |
| `ship.go` | Pushing and opening the PR after the last step |
| `lifecycle.go` | `prf resume` and `prf clean` |
| `config.go` | The home folder, `conf.json`, `config` and `config init` |
| `roles.go`, `rolecmd.go` | Loading and validating roles; `prf role` |
| `pipeline.go` | `pipeline.json` and `prf pipeline` |
| `context.go`, `index.go` | Preparing `<job>/context`: setup commands, the codebase map, and `prf index` |
| `sandbox.go`, `tools.go` | The sandbox settings file; mise install and trust |
| `jobenv.go` | `job_env` and `job_seed`: the job's `mise.toml`, seed copies, and the folders steps may write |
| `roles/*.md` | The built-in roles, installed by `config init` |
