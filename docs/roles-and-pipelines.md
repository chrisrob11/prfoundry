# Roles and pipelines

A **role** is one step: a `<name>.md` prompt that Claude runs as a fresh session. A **pipeline**
is the path a branch takes: an ordered list of roles, and optionally [gates](gates.md).
Pipelines are defined per repo in [`pipeline.json`](configuration.md#pipelinejson).

Roles run one after another in the same checkout. Each commits its own work, so the branch history
reads implement, then test, then review. The run stops at the first role that fails.

## Which roles run

1. `--role a,b`, if given;
2. otherwise `--pipeline <name>` from the repo's `pipeline.json`;
3. otherwise the repo's default pipeline.

`apply` stops after step 2: without `--role` or `--pipeline` it sends `--prompt` to Claude on its
own, with no role. `--role` and `--pipeline` can't be combined.

## Which model each step uses

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

## Where roles come from

The repo's `~/.prf/<repo>/roles/` first, then the shared `~/.prf/roles/`. So a repo can override
`review` without affecting others.

## What a step must leave behind

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
[`@ask`](gates.md#questions-from-a-step) gate after it a question isn't checked; its re-run, once
answered, is. In `prf apply`, a checkout with uncommitted changes before the job starts breaks
every expectation.

## Built-in roles

| Built-in role | What it does |
|---|---|
| `implement` | Implements the PR spec and/or prompt, runs the build and tests, and commits. Needs `--pr` or `--prompt` |
| `test` | Test Engineer: covers every promised behavior with table-driven tests, runs `go test -race ./...`, fixes real bugs as separate `fix:` commits, never weakens a test |
| `review` | Idiomatic Go Reviewer: hunts AI slop (restating comments, needless interfaces, impossible nil checks, sloppy errors, vague names, dead code) and fixes it without changing behavior |
| `answer-question` | Question Recorder: the [`@ask`](gates.md#questions-from-a-step) gate's fix role. Records your answers in the PR spec, if there is one, and deletes the question file |
| `address-review` | Review Responder: the [`@review`](gates.md#human-review) gate's fix role. Makes the changes your review asks for (or says why not), commits them as `review: ...`, and replies to each comment |
| `fix-check` | Check Fixer: the [`@check`](gates.md#checks) gate's fix role. Finds the cause of the failure, fixes it, runs the command again, and commits as `fix: ...`; never weakens a test to pass |

`prf config init` installs these into `~/.prf/roles/`. They're ordinary files, so edit them
freely; a second `init` won't overwrite them, and `prf role reset <name>` restores the original.

## Writing a role

A role is a Go [text/template](https://pkg.go.dev/text/template) that can use:

| Field | Value |
|---|---|
| `{{.PR}}` | Absolute path to the PR spec, or empty |
| `{{.Branch}}` | The branch being worked on |
| `{{.Task}}` | The `--prompt` text, or empty. It's also appended as `## Task`, so roles rarely need it |
| `{{.Review}}` | Only in a [`@review`](gates.md#human-review) gate's fix role: your review's comments, as markdown. In an [`@ask`](gates.md#questions-from-a-step) gate's: your answers. In a [`@check`](gates.md#checks) gate's: the command and what failed. Empty otherwise |
| `{{.Question}}` | Only in an [`@ask`](gates.md#questions-from-a-step) gate's fix role: the question you answered. Empty otherwise |
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

## Managing roles and pipelines

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
