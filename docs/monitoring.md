# Monitoring

## The job folder

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
into `-`. A job folder also holds its [tmp folder](how-it-works.md#the-jobs-tmp-folder) and, as
gates run, `reviews/`, `asks/` and `checks/` (see [gates](gates.md)).

## Progress output

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

## prf status

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
| `review` | Waiting at a [`@review`](gates.md#human-review) gate for you; DETAIL is the review page's URL |
| `ask` | Waiting at an [`@ask`](gates.md#questions-from-a-step) gate for your answers; STAGE is `ask (waiting for owner)`, DETAIL the page's URL |
| `paused` | Claude's usage limit stopped a step. The job waits until 5 minutes past the reset the error names (30 minutes if it names none), then runs that step again; DETAIL says when. The status file gets a `PRF_PAUSE ... until=<time>` line. Any other failure still fails the job |
| `done` | Every role succeeded |
| `failed` | A role or setup step failed; STAGE is where it stopped |
| `died` | It says it's running, but its prf process is gone (killed, machine restarted) |

`--repo app` shows one repo's jobs, and `--workdir` reads another workdir. `--json` prints the
full state of each job, the same as its `state.json`:

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
successfully or not, for a [re-run](how-it-works.md#what-claude-is-given) of that step. Once
finished, a job also has `exit`, and after shipping `pushed: true` and `pr_url`. `pr`, `task`,
`push` and `pr_create` are what `prf resume` needs to carry on the same way. `spend` is what
each role cost, by role and model, summed over every run of it (fix rounds, re-runs and resumes):
`runs`, `turns`, `cost`, and `cache_read_tokens`, the context Claude re-read across those turns.

`state.json` is rewritten at every change, and every 2 seconds while Claude is working, and it's
always replaced in one step, so a reader never sees half a file. `exit` appears once the job has
finished.

To watch a running wave: `while sleep 5; do clear; prf status; done`.

## prf costs

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

## Status lines

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
  started (see [cost caps](orchestrating.md#cost-caps)).
- `PRF_PHASE ... exit=10 ... expect=commits`: the role broke its
  [expectation](roles-and-pipelines.md#what-a-step-must-leave-behind); the log says how.
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
- `PRF_PAUSE ... until=<time>`: Claude's usage limit stopped a step, which runs again then.
- `PRF_PUSH branch=... exit=...` and `PRF_PR branch=... url=...` (or `exit=1`): prf pushed
  and opened the PR, after the last step and before `PRF_DONE`.
- `PRF_DONE`: the job finished. It's always the last line written. `phase` is the last role
  that ran, or `setup` if the job failed before any role. `dir` is where the work is.

The file is appended to, so a [resumed](orchestrating.md#re-running-resuming-and-cleaning-up) job
adds to it. `--status` writes these lines to another file instead. For what each exit code means,
see [exit codes](orchestrating.md#exit-codes).
