# Gates

A gate is a pipeline step that isn't a role: `@review` waits for your review, `@ask` lets the role
before it ask you a question, and `@check` runs a command and loops a fix role until it passes.
In `pipeline.json` a gate is `"@review"`, `"@ask"` or `"@check"`, or an object with `"gate"` and
its options (below). On the command line it's `@review`, or `@review:model` to pick its fix
role's model, e.g. `--role implement:opus,@review:sonnet`.

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
the job is [resumed](orchestrating.md#re-running-resuming-and-cleaning-up): its port and token are
saved in `<job>/reviews/link.json`. Only if that port has been taken meanwhile does the page move
to a new one, and prf says so. `prf status` shows a waiting job as `review`, with its link, and
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

`review_open: false` in `conf.json` (or `--review-open=false`) keeps the browser closed and only
notifies you.

Every round is saved in `<job>/reviews/`: `round-N.json` (your verdict, comments, the commit you
reviewed, Claude's replies and summary) and `round-N.md` (the review as the fix role saw it). A
gate that's [resumed](orchestrating.md#re-running-resuming-and-cleaning-up) carries on from these:
if the job stopped before Claude addressed a round, it does that first, then waits for your next
review.

## Questions from a step

Sometimes a role hits a question only you can answer: a design decision the brief didn't settle,
or a change outside its scope. Put `@ask` right after it, and the role can stop and ask:

```json
"feature": ["implement", "@ask", "test", "@review"]
```

The role writes its question, as markdown, to `$PRF_TMP/stop.md` and finishes. (Tell it to in
the role's text or the brief.) `PRF_TMP` is the job's own
[tmp folder](how-it-works.md#the-jobs-tmp-folder). Then the `@ask` gate:

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
[resumed](orchestrating.md#re-running-resuming-and-cleaning-up) gate carries on from these: it
records answers the job stopped before recording, re-runs the role if it hadn't yet, and asks
again if the question file is still there (after **Stop job**, for example).

## Checks

A role saying "the tests pass" isn't the same as the tests passing. `@check` checks for real, and
loops until the check passes:

```json
"feature": ["implement", "test", "@check", "review"]
```

`@check` runs a shell command in the checkout and passes when it exits 0. When it fails, its fix
role (`fix-check`) gets the command and what failed in `{{.Review}}`, fixes the cause, and
commits. Then the gate runs the command again. After `max_rounds` fixes that still fail, the job
stops with exit 9.

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

### What the fix role reads

Not the whole output, which can be thousands of lines, nor just its end, which can miss a failure
printed early. When the output is Go's, prf picks out what failed, wherever it is: build, vet and
lint errors (`file.go:12:5: ...`, each once), each failed test with its log (with or without
`-v`), panics with the failing goroutine's stack, data race reports, and the `FAIL` package lines.
Passing tests and packages are left out. Each failure is capped at 60 lines, with 20 failures and
400 lines in all, keeping the first ones. Any lines at the end of the output that the excerpt
doesn't already show (a `make` error, say) are added after it. For output that isn't Go's, it gets
the last 200 lines. Either way, the prompt says where the full output is, in the job's
[tmp folder](how-it-works.md#the-jobs-tmp-folder), for the role to read if it needs more.

### It runs in Claude's sandbox

The command runs the code the steps wrote, so when the job is sandboxed it runs in the same
[sandbox](sandbox.md) as Claude, with the same limits from `claude-settings.json`: the same
network allowlist (and local binding and Unix sockets, so a test that starts a server on localhost
passes here when it passes in Claude's shell), the same unreadable folders (including `Read(...)`
and `Edit(...)` rules in its `permissions.deny`), and writes only to the checkout, the job's tmp
folder and the tool caches. The checkout's `.claude/` and `.mcp.json` stay unwritable, so the
command can't plant settings or a hook that the next Claude step would load. prf uses
[`srt`](https://github.com/anthropics/sandbox-runtime), Anthropic's sandbox runtime and the one
Claude Code itself uses, to do it. Install it with `npm install -g @anthropic-ai/sandbox-runtime`.
If it isn't installed, `@check` stops the job (exit 3) rather than run the command unsandboxed. A
job run with `--sandbox=false`, or whose Claude settings turn the sandbox off, runs the command
directly, as you.

### How it runs

The command runs with `sh -c`, sandboxed or not, with a short `TMPDIR` of its own (in your temp
folder, removed when the gate ends), as it would have if you ran it, and `PRF_TMP` set to the
job's tmp folder. It gets a process group of its own. When it times out, or the job is stopped
(Ctrl-C, `kill`, or the terminal closing), the whole group gets `SIGTERM`, then `SIGKILL` 5
seconds later, so tests can clean up. When the command ends, anything it left running in the
background is killed. Its exit code decides, even if a leftover process kept its output open. A
command killed by a signal shows as exit 128 plus the signal.

The command's output goes to `claude.log` as it runs, and each run's output is saved in
`<job>/checks/step-N/round-M.log`, with the failure as the fix role saw it in `round-M.md`. A
[resumed](orchestrating.md#re-running-resuming-and-cleaning-up) gate runs the command again,
numbering its rounds after the saved ones, with a fresh set of fixes. It uses the step's `run` as
the job was started with, or the repo's `check` as `pipeline.json` says now.
