# Orchestrating

prf is meant to be called by another program, often a small local LLM, that reads a plan and
starts a wave of PRs: one `prf run` per PR, each in its own clone and branch.

## Driving prf from an orchestrator

What an orchestrator needs to know:

- **Start a job:** `prf run --repo <alias> --pr <name>`, one per PR. Run them in the
  background to run a wave in parallel; each has its own clone and branch.
- **Know where every job is:** `prf status --json`. Each entry has `state` (setup, running,
  review, ask, paused, done, failed, died), `step` and `roles` (step 2 of 5), `stage` (the role or
  setup step), and `activity` (Claude's latest action, updated every 2 seconds). For one job, read
  its `state.json` directly. See [prf status](monitoring.md#prf-status).
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
  See [managing roles and pipelines](roles-and-pipelines.md#managing-roles-and-pipelines).
- **Pick models:** set them per step in the pipeline, or pass `--model <model>` to run every step of
  one job on the same model (handy for a cheap trial run).
- **Don't parse anything else.** Log lines starting `prf:` are for people and may change; the
  JSON output, `state.json`, and the [`PRF_*` lines](monitoring.md#status-lines) are the stable
  interface.

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

Claude is never allowed to push: the default [sandbox](sandbox.md) settings deny `git push` and
`gh`, plain and `rtk`-prefixed. Shipping is prf's job, so "only pushed if every step passed" is a
guarantee of the program, not a request in a prompt.

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
[`prf costs`](monitoring.md#prf-costs) shows where the money went.

## Exit codes

| Exit code | Meaning |
|---|---|
| `0` | Every role succeeded |
| `1`, or another non-zero code | A role failed: its Claude exit code, or `1` if Claude exited cleanly but marked its result as an error. Later roles didn't run |
| `2` | Usage error: bad flags, unknown role or pipeline, missing PR file, invalid name. Nothing was cloned |
| `3` | Setup failed: the job's clone already exists, the workdir's disk is fuller than `max_disk_percent`, clone or branch failed, a setup command failed, or a file couldn't be written. Also a `@check` command that can't start or be sandboxed (no `srt`) |
| `4` | Every step passed, but pushing or opening the PR failed: the work is done but didn't ship |
| `5` | A [cost cap](#cost-caps) stopped the job |
| `6` | A [`@review`](gates.md#human-review) gate ran `max_rounds` rounds and you still requested changes. `prf resume` carries on |
| `7` | An [`@ask`](gates.md#questions-from-a-step) gate took `max_rounds` questions and the role asked again. `prf resume` carries on |
| `8` | You chose **Stop job** at an [`@ask`](gates.md#questions-from-a-step) gate. `prf resume` asks the question again |
| `9` | A [`@check`](gates.md#checks) gate's command still failed after `max_rounds` fixes. `prf resume` runs it again |
| `10` | A role didn't leave the checkout as its [`expect`](roles-and-pipelines.md#what-a-step-must-leave-behind) says: no commits, commits it shouldn't have made, or uncommitted changes. `prf resume` runs it again |
| `130` | The job was interrupted (Ctrl-C, `kill`, or its terminal closing), or Claude was killed by a signal |

Exit 0 means Claude finished without reporting an error, not that the work is right. Check the
commits and Claude's summaries. On Ctrl-C, Claude gets the signal and prf still writes
`PRF_DONE`.

## Re-running, resuming and cleaning up

- **Resume a failed or died job:** `prf resume <repo>/<job>` (as `prf status` shows it, or a
  job folder's path) re-runs from the step that stopped, in the same checkout, with the same PR
  spec, prompt, models and shipping settings, and the current role files. Setup isn't repeated.
  Steps keep their numbers (step 2/3 stays 2/3), status lines are appended, and the cost so far
  counts toward `--max-cost`. A step that ran before is told where its last run ended and where
  its report is (see [what Claude is given](how-it-works.md#what-claude-is-given)). `--from <step>`
  starts elsewhere, including in a finished job; `--model`, `--max-cost`, `--max-step-cost`,
  `--push`, `--pr-create`, `--review-open`, `--mode`, `--sandbox`, `--claude` and `-v` work as in
  `run`.
- **Start over:** `run` refuses a job whose clone already exists (exit 3) rather than reuse it,
  and leaves that job's status and `state.json` untouched. Remove the job (below) or use a
  different `--branch`/`--pr`.
- **Clean up:** `prf clean` lists the job folders it would remove: jobs that are done
  (`--failed` adds failed and died ones), never running ones. It keeps any job whose clone has
  uncommitted changes or commits on no remote, saying why, unless you add `--force`. Add `--yes`
  to remove them; it reports the space freed. `--repo` limits it to one repo.
- **Try a role on your own checkout:** `prf apply --repo app --role review` (with the alias's
  `path`), or `prf apply --dir ~/code/app --role review`.
