# How it works

- `prf run` clones a repo into a fresh job folder, creates a branch, and runs a pipeline there.
- `prf apply` runs a pipeline, or a single prompt, in a checkout you already have.
- `prf status` shows every job: which step of its pipeline it's on and what Claude is doing.
- `prf role` and `prf pipeline` list, show and change roles and pipelines, with validation,
  so you or an LLM can keep them set up the way you want.
- `prf index` builds the codebase map for a checkout, to preview what Claude is given.
- `prf config` shows the settings in effect; `prf config init` sets up `~/.prf`.

A pipeline can stop for you: a `@review` step opens a pull-request-style review of the branch in
your browser, and Claude addresses your comments, round after round, until you approve. A role can
stop to ask you something: an `@ask` step after it shows you its question beside the diff,
records your answers, and runs the role again. And a pipeline can check the work for real instead
of taking a role's word for it: `@check` runs a command, like the tests, and when it fails, a fix
role gets the output and fixes the branch, and the gate runs it again. See [gates](gates.md).

Claude never pushes or opens PRs: the sandbox denies it `git push` and `gh`. Each role commits its
work on the branch. With `--push` (and `--pr-create`), prf itself pushes the branch and opens a
PR once every step has passed; see [pushing and opening PRs](orchestrating.md#pushing-and-opening-prs).

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
   prf writes `<workdir>/app/login/mise.toml` and makes the copies. Then, if the repo has a mise
   config, `mise install` runs (see [mise](sandbox.md#mise)).
4. **Prepare context.** The [codebase map](codebase-map.md) is built and the repo's `setup`
   commands run, filling `<workdir>/app/login/context/`.
5. **Run each role in order.** Each role is a fresh `claude -p` call in the clone, on the model
   its step names (see [which model each step uses](roles-and-pipelines.md#which-model-each-step-uses)).
   The run stops at the first role that fails: one that exits non-zero, or whose result Claude
   marks as an error, or that doesn't leave the checkout as its
   [`expect`](roles-and-pipelines.md#what-a-step-must-leave-behind) says. A `@review` step waits
   for your [review](gates.md#human-review) instead, until you approve. An `@ask` step shows you
   the [question](gates.md#questions-from-a-step) the role before it left, if it left one.
6. **Ship, if asked.** With `--push`, once every step has passed, prf pushes the branch and,
   with `--pr-create`, opens the PR (see [pushing and opening PRs](orchestrating.md#pushing-and-opening-prs)).
7. **Report.** A `PRF_DONE` line goes to the status file and stderr, `state.json` says done or
   failed, and prf exits with the last role's exit code.

Every step is reported as it happens; see [monitoring](monitoring.md).

`prf apply` does the same in an existing checkout, skipping the clone (step 2), except that
`--branch` switches to or creates that branch first.

## What Claude is given

Each role runs as:

```
claude -p <prompt> --permission-mode <mode> --output-format stream-json --verbose [--model <model>]
       [--add-dir <PR spec's folder>] [--add-dir <job>/context] [--settings <claude-settings.json>]
       <claude_args from conf.json> <anything after -- on the prf command line>
```

in the clone's directory, with stdin from `--in` if given. prf reads Claude's stream of events to
report progress, so don't override `--output-format` in `claude_args`. The prompt is built from
these parts:

1. The role's text, rendered as a template (see [writing a role](roles-and-pipelines.md#writing-a-role)).
   With no role (`apply --prompt` only), this is the `--prompt` text itself.
2. A `## Codebase context` section listing what's in `<job>/context/`, asking Claude to read
   `index.md` first and open the `packages/` files it needs.
3. A `## Task` section with the `--prompt` text, when a role is used and `--prompt` was given.
4. A `## This step ran before` section, when the role's step already ran in this job (it failed
   and you [resumed](orchestrating.md#re-running-resuming-and-cleaning-up) the job, say): "This
   step ran before, ending at `<sha>`: read `git diff <sha>..HEAD` and its earlier report first,
   and read unchanged code only where you need it." The report is that run's final message, kept
   in `$PRF_TMP/reports/step-N.md`. So a re-run picks up from what it did instead of starting
   over. Gates' fix roles don't get it.

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
makes it and lets steps write to it inside the [sandbox](sandbox.md), for Claude and for
[`@check`](gates.md#checks) commands. Nothing else in the job folder becomes writable. Only `run`
writes `mise.toml` and seeds; `resume`, and `apply` in a `run` clone, keep the job's file and its
folders. `apply` in your own checkout doesn't use them at all.
