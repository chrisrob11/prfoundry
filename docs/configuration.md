# Configuration

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

`prf config init` writes a starter `conf.json`, the built-in roles and `claude-settings.json`,
skipping any that already exist. `prf config` prints the home folder, which repos have a
`pipeline.json` or `roles/` there, and the `conf.json` settings in effect, as JSON.

**Repo names.** `<repo>` is the name prf knows a repo by. It's the alias you pass to `--repo`
if it's in `conf.json`. Otherwise, for `run` it's the clone URL's last part without `.git`
(`git@github.com:me/app.git` is `app`), and for `apply` it's `--repo` if given, else the
checkout's folder name. Names may only use letters, digits, `-`, `_` and `.`; add an alias for a
repo whose name doesn't fit.

## conf.json

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
| `agent` | The coding agent steps run: `"claude"` (the default) or `"opencode"`; see [the agent](#the-agent-claude-or-opencode) |
| `mode` | Claude permission mode (default `acceptEdits`) |
| `model` | Model for steps that don't name one, in any repo without its own `model` (default: Claude's own) |
| `claude` | Claude binary (default `claude`) |
| `claude_args` | Extra arguments for every Claude call, placed before anything after `--`. Use `model` rather than `--model` here, or it overrides every step's model |
| `opencode`, `opencode_args`, `opencode_model` | opencode's binary (default `opencode`), extra arguments for every opencode call, and its model for steps that don't name one |
| `max_cost`, `max_step_cost` | Default [cost caps](orchestrating.md#cost-caps) in USD for the whole job and for each step |
| `sandbox` | `false` turns the [sandbox](sandbox.md) off (default on) |
| `review_open` | `false` stops a [`@review`](gates.md#human-review) or [`@ask`](gates.md#questions-from-a-step) step from opening its page in your browser; you still get a notification (default `true`) |
| `max_disk_percent` | `run` refuses to clone (exit 3) when the disk holding the workdir is at least this full, in percent used as `df` counts it, e.g. `90`. Default `0`: no check |
| `repos` | Aliases: a URL string, or an object (below) |

A repo alias object can have:

| Key | Meaning |
|---|---|
| `url` | What `run` clones |
| `agent` | `"claude"` or `"opencode"` for this repo's jobs, over the global `agent` |
| `path` | Your local checkout: what `apply` works in, and where `run` copies git objects from |
| `base` | The branch to clone and branch from, and the PR's base |
| `pr_dir` | Overrides the global `pr_dir` |
| `push` | `true` pushes the branch once every step passes |
| `pr_create` | `"draft"` or `"ready"`: open a PR after pushing (implies `push`) |
| `pr_title_file`, `pr_body_file` | Files in the checkout (e.g. written by a role) with the PR's title and body. Without them, gh fills both from the commits |
| `job_env` | Environment variables for the job, as `{"NAME": "value"}`, written to `<job>/mise.toml` (see [the job's environment](how-it-works.md#the-jobs-environment)). `{{job}}` in a value is the job folder |
| `job_seed` | Copies made into a fresh job folder before the first step, as `[{"from": "~/shared", "to": "{{job}}/copy"}]`. `to` must be under `{{job}}` |

`~` is expanded in paths. Unknown keys are an error, so a typo can't silently do nothing.
A non-empty flag beats an env var, which beats `conf.json`, which beats the built-in default.

## The agent: claude or opencode

Each step runs Claude Code unless the job's agent is [opencode](https://opencode.ai): `--agent
opencode` on `run`, `apply` or `resume`, else the repo alias's `agent`, else `conf.json`'s.
`state.json` records it, so `resume` keeps it unless you give `--agent`. Gates and fix roles run
on the same agent, and work the same way.

```json
{
  "agent": "opencode",
  "sandbox": false,
  "mode": "bypassPermissions",
  "opencode_model": "anthropic/claude-sonnet-5-5"
}
```

What differs on opencode:

| Setting | On opencode |
|---|---|
| Models | `opencode_model` (conf.json, pipeline.json, a step) and `--model` are `provider/model`, as `opencode models` lists them. `model` and `--role role:model` name Claude models: a step's `model` is ignored, and `--role role:model` is refused |
| `--sandbox` | Refused: opencode has no sandbox. Run it with `--sandbox=false` or `"sandbox": false`, so `@check` commands run unsandboxed too |
| `--mode` | Mapped to opencode's permission rules: `default` and `dontAsk` allow reads only, `acceptEdits` reads and edits, `plan` reads and denies edits and shell commands, and `bypassPermissions` approves everything else (`--auto`). What a mode doesn't allow, such as a shell command under `acceptEdits`, opencode refuses, since nobody can approve it; to let a step run the tests, use `bypassPermissions`. `auto` is refused. In every mode `git push`, `gh` and writing opencode's own config are denied |
| Budgets | opencode has no budget of its own: prf adds up the cost opencode reports after each model call and stops the run (exit 5) once it reaches the step's budget, so a step can overshoot by one call. A model opencode prices at $0, like a subscription or a local one, is never stopped. opencode doesn't always report its last call, so costs can be low by one call |
| Usage limits | A rate limit, or a workspace's usage limit, pauses the job like Claude's: until the time opencode gives, or 30 minutes. A free-tier or per-model spend limit fails the step, since waiting won't help |
| Logs | `claude.log` and `claude.jsonl` hold opencode's steps and events too |

`opencode_args` and anything after `--` go to `opencode run`: `["--agent", "plan"]` picks an
opencode agent. A repo's own `opencode.json` still applies. prf's permission rules come after its
top-level `permissions`, but an agent's own `permissions` there come later still and win, so
check a repo's `opencode.json` before running it.

## pipeline.json

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
| `pipelines` | Named lists of steps, run in order. A step is a role name, or `{"role": ..., "model": ..., "expect": ...}` to pick its model or what it must leave behind, or a [`@review`](gates.md#human-review), [`@ask`](gates.md#questions-from-a-step) or [`@check`](gates.md#checks) gate |
| `default` | The pipeline that runs when `--pipeline` isn't given |
| `model` | Model for this repo's steps that don't name one (overrides `conf.json`'s `model`) |
| `opencode_model` | The same for opencode, as `provider/model`. A step object can set its own `opencode_model` too |
| `expect` | What this repo's role steps must leave behind, unless a step object sets its own `expect`: `"commits"`, `"read-only"` or `"clean"` (see [what a step must leave behind](roles-and-pipelines.md#what-a-step-must-leave-behind)). Default: no check |
| `max_cost`, `max_step_cost` | This repo's [cost caps](orchestrating.md#cost-caps), overriding `conf.json`'s. A step object can set its own `max_cost` too |
| `index` | `false` skips the built-in [codebase map](codebase-map.md) (default `true`) |
| `setup` | Shell commands run in the checkout before the first role (see [codebase map](codebase-map.md#setup-commands)) |
| `check` | The command a [`@check`](gates.md#checks) gate runs when it doesn't name its own |

A repo without a `pipeline.json` has one built-in pipeline, `default`: `implement -> test -> review`.
Unknown keys are an error. You can edit the file by hand or use
[`prf pipeline`](roles-and-pipelines.md#managing-roles-and-pipelines), which validates before
saving.

## Environment variables

| Variable | Used for |
|---|---|
| `PRF_HOME` | The prf home (same as `--home`) |
| `PRF_WORKDIR` | Where job folders go (same as `--workdir`) |
| `CLAUDE_BIN` | The Claude binary (same as `--claude`) |
| `OPENCODE_BIN` | The opencode binary (same as `--opencode`) |

Every step gets `PRF_TMP`, the job's [tmp folder](how-it-works.md#the-jobs-tmp-folder).

Setup commands also get:

| Variable | Value |
|---|---|
| `PRF_CONTEXT` | `<job>/context`: write files here to hand them to Claude |
| `PRF_JOB` | The job folder |
| `MISE_TRUSTED_CONFIG_PATHS` | The workdir, prepended to any value you set (`run` only; Claude gets it too) |
