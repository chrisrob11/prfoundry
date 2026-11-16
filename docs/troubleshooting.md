# Troubleshooting

| Problem | Fix |
|---|---|
| `no role "implement" in ...` | Run `prf config init` to install the built-in roles |
| `can't use "x" as a repo name` | The URL or folder name has characters prf can't use as a folder name. Add an alias in `conf.json` (see [repo names](configuration.md#the-prf-home)) |
| `destination already exists` (exit 3) | That job ran before. See [re-running](orchestrating.md#re-running-resuming-and-cleaning-up) |
| `the disk holding ... is 93% full` (exit 3) | The workdir's disk is past `max_disk_percent`. `prf clean` frees finished jobs' space |
| A command fails with `Operation not permitted` in `claude.log` | The [sandbox](sandbox.md) blocked a write. Add the folder to `sandbox.filesystem.allowWrite` in `claude-settings.json` |
| `CONNECT tunnel failed, response 403` or "host is not on the allow list" | The sandbox blocked a host. Add it to `sandbox.network.allowedDomains` |
| `x509: OSStatus -26276` from Go | Go's TLS check in the macOS sandbox. Make sure `enableWeakerNetworkIsolation` is `true` |
| mise says a config is "not trusted" | For `run` this is handled; for `apply`, run `mise trust` in your checkout (see [mise](sandbox.md#mise)) |
| `claude.log` says a command was denied or needed approval | Headless Claude can't ask, so anything not auto-approved is refused. That happens to shell commands when the sandbox is off, and to edits outside the checkout. Keep the sandbox on, or use `--mode bypassPermissions` in a throwaway clone |
| Sandbox fixes in a new prf version don't show up (e.g. Claude can still `git push`) | prf never rewrites `claude-settings.json`. Delete it and the next run (or `config init`) writes the new defaults; copy back anything you'd added |
| `@check` stops the job with exit 3 | `srt` isn't installed: `npm install -g @anthropic-ai/sandbox-runtime` (see [checks](gates.md#it-runs-in-claudes-sandbox)) |
| `unknown field` in `conf.json` or `pipeline.json` | A typo in a key. The message names it |
