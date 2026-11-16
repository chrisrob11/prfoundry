# Sandbox

Claude runs inside Claude Code's sandbox by default. prf passes `--settings` with
`~/.prf/<repo>/claude-settings.json` if it exists, otherwise `~/.prf/claude-settings.json`.
It writes the shared one with the defaults below the first time it's needed (or on
`prf config init`), and never rewrites it.

It's an ordinary [Claude Code settings file](https://code.claude.com/docs/en/settings), so edit it
to change what Claude can reach. The defaults:

- **Prompts:** shell commands run without approval prompts.
- **Writes:** only to the job's checkout, its [tmp folder](how-it-works.md#the-jobs-tmp-folder),
  the folders its [`job_env` and `job_seed`](how-it-works.md#the-jobs-environment) name, the Go
  caches (`GOMODCACHE`, `GOCACHE`, `$GOPATH/pkg/sumdb`, as `go env` reports them when the settings
  file is first written) and mise's state folder.
- **Network:** only GitHub (`github.com`, `*.github.com`, `*.githubusercontent.com`) and the Go
  module proxy and checksum servers (`proxy.golang.org`, `sum.golang.org`,
  `storage.googleapis.com`). Any other host is refused, not prompted for.
- **Credentials:** `~/.ssh`, `~/.aws`, `~/.gnupg`, `~/.config/gh`, `~/.netrc` and `~/.docker`
  can't be read, by shell commands or by Claude's Read tool.
- **No pushing:** `git push` and `gh` are denied, plain and `rtk`-prefixed (the rtk hook rewrites
  commands before the check, so both forms matter). prf pushes itself; see
  [pushing and opening PRs](orchestrating.md#pushing-and-opening-prs).
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

[`@check`](gates.md#checks) commands run in the same sandbox as Claude, through `srt`; see
[it runs in Claude's sandbox](gates.md#it-runs-in-claudes-sandbox).

## mise

If the repo has a mise config (`mise.toml`, `.tool-versions`, ...) and mise is installed, prf
runs `mise install` in the checkout before Claude starts. That happens outside the sandbox,
because the sandbox blocks mise from downloading tool versions. A failed install is only a
warning.

For `run`, prf also sets `MISE_TRUSTED_CONFIG_PATHS` to the workdir, for that install, the
setup commands, and Claude. Fresh clones under the workdir, and the `mise.toml` prf writes
beside them from [`job_env`](how-it-works.md#the-jobs-environment), are then trusted for these
runs only, without `mise trust` and without changing your mise settings. `resume`, and `apply` in
a clone under the workdir, do the same. `apply` in your own checkout doesn't, so mise's trust
stays as you set it. To use a job's environment from your own shell, run
`mise trust <job>/mise.toml` once (and the clone's own config, if it has one), or add the workdir
to mise's `trusted_config_paths`.
