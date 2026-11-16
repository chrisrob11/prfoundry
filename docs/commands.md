# Command reference

`prf [--home <dir>] <command> ...`. The global `--home` (env `PRF_HOME`, default
`~/.prf`) can go before or after the command. Every command has `--help`.

| Command | What it does |
|---|---|
| [`prf run`](#prf-run) | Clone a repo into a fresh job folder, switch to a branch, and run a pipeline |
| [`prf apply`](#prf-apply) | Run a pipeline, roles, or a single prompt in an existing checkout |
| [`prf status`](#prf-status) | Every job's state, step, stage and latest activity |
| [`prf costs`](#prf-costs) | What each role has cost across jobs, by role and model |
| [`prf resume`](#prf-resume) | Re-run a failed or died job from where it stopped |
| [`prf clean`](#prf-clean) | Remove finished job folders |
| [`prf role`, `prf pipeline`](#prf-role-prf-pipeline) | List, show and change roles and pipelines |
| [`prf index`](#prf-index) | Build the codebase map for a checkout, without running Claude |
| [`prf config`](#prf-config) | Show the settings in effect; `init` sets up the home |
| [`prf version`](#prf-version) | prf's version, commit and build date |

## prf run

Clone a repo into a fresh job folder, switch to a branch, and run a pipeline.

| Flag | Meaning |
|---|---|
| `--repo` | Repo URL, local path, or alias from `conf.json` (required) |
| `--branch` | Branch to create, or switch to if it exists on the remote. Default `<branch_prefix><pr name>`; required without `--pr` |
| `--base` | Branch to clone and branch from (default: the alias's `base`, else the remote's default branch) |

Plus the [shared flags](#shared-flags). See [how a job runs](how-it-works.md#how-a-job-runs).

## prf apply

Run a pipeline, a list of roles, or a single prompt in an existing checkout.

| Flag | Meaning |
|---|---|
| `--repo` | The repo's name: picks its pipelines, roles and job folder. An alias's `path` is used as the checkout unless `--dir` is given |
| `--dir` | The checkout to work in (default: the current directory) |
| `--branch` | Switch to this branch first, creating it if needed (default: stay on the current branch) |

Plus the [shared flags](#shared-flags). `apply` warns if the working tree has uncommitted
changes, because Claude's commits may include them.

## Shared flags

| Flag | Meaning |
|---|---|
| `--pr` | PR spec file. Tried as given, then as `<pr_dir>/<name>` and `<pr_dir>/<name>.md` |
| `--prompt` | Task text. Added to every role's prompt; with no role, it is the whole prompt |
| `--pipeline` | Pipeline from the repo's `pipeline.json` (`run` default: the repo's default pipeline; `apply` default: none) |
| `--role` | Roles to run in order instead of a pipeline, comma-separated or repeated. `role:model` picks a step's model, e.g. `--role implement:opus,review:haiku` |
| `--model` | Model for every step, overriding the pipeline's (see [which model each step uses](roles-and-pipelines.md#which-model-each-step-uses)) |
| `--max-cost`, `--max-step-cost` | [Cost caps](orchestrating.md#cost-caps) in USD for the job and for every step |
| `--push` | Push the branch once every step passes; `--push=false` skips it once |
| `--pr-create` | After pushing, open a `draft` or `ready` PR; `none` skips it once |
| `--workdir` | Where job folders go (env `PRF_WORKDIR`; default `conf.json`'s `workdir`, else `~/prf-work`) |
| `-v`, `--verbose` | Also print every step Claude takes (files read, commands, edits, messages) as it happens |
| `--out` | Write the step log to this file instead of `<job>/claude.log`, and keep Claude's summaries off stdout |
| `--in` | Extra context on Claude's stdin. A regular file goes to every role; a fifo or `-` (prf's own stdin) can only be read once, so it goes to the first role |
| `--status` | Append status lines to this file instead of `<job>/status` |
| `--mode` | Claude permission mode (default `conf.json`'s `mode`, else `acceptEdits`) |
| `--sandbox` | Run in Claude Code's [sandbox](sandbox.md) (default on; `--sandbox=false` turns it off) |
| `--review-open` | At a [`@review`](gates.md#human-review) or [`@ask`](gates.md#questions-from-a-step) gate, open its page in your browser (default `conf.json`'s `review_open`, else on; `--review-open=false` only notifies) |
| `--claude` | Claude binary (env `CLAUDE_BIN`; default `conf.json`'s `claude`, else `claude`) |
| `-- ...` | Everything after `--` is passed to `claude`, e.g. `-- --model opus` |

## prf status

`prf status [--repo <name>] [--json] [--workdir <dir>]`: every job's state, step, stage and
latest activity. See [prf status](monitoring.md#prf-status).

## prf costs

`prf costs [--repo <name>] [--since <duration>] [--json] [--workdir <dir>]`: every job's spend,
summed by role and model, most expensive first. See [prf costs](monitoring.md#prf-costs).

## prf resume

`prf resume <repo>/<job> [--from <step>] [flags] [-- extra claude args]`: re-run a failed or
died job from where it stopped. Its flags are `--from`, `--workdir`, `-v`, `--model`,
`--max-cost`, `--max-step-cost`, `--push`, `--pr-create`, `--review-open`, `--mode`, `--sandbox`
and `--claude`. See [resuming](orchestrating.md#re-running-resuming-and-cleaning-up).

## prf clean

`prf clean [--repo <name>] [--failed] [--force] [--yes] [--workdir <dir>]`: remove finished job
folders; a dry run unless `--yes`. See [cleaning up](orchestrating.md#re-running-resuming-and-cleaning-up).

## prf index

`prf index [--dir <checkout>] [--out <folder>]`: build the [codebase map](codebase-map.md) for a
checkout (default: the current directory, into `./codebase-map`), without running Claude.

## prf role, prf pipeline

`prf role list|show|set|remove|reset` and `prf pipeline list|show|set|remove|default`. See
[managing roles and pipelines](roles-and-pipelines.md#managing-roles-and-pipelines).

## prf config

`prf config` prints the home folder, which repos have a `pipeline.json` or `roles/` there, and
the `conf.json` settings in effect, as JSON. `prf config init` writes a starter `conf.json`, the
built-in roles and `claude-settings.json`, skipping any that already exist. See
[configuration](configuration.md).

## prf version

`prf version`, or `prf --version`: print prf's version, the commit it was built from and when.
A release build has them stamped in; a `go install …@vX.Y.Z` reads them from Go's build info.
