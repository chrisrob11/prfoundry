# Contributing

Thanks for helping with prf.

## Build and test

```sh
mise install   # the Go version this repo pins
make ci        # fmt-check, lint, tests with a coverage floor, build: exactly what CI runs
```

`make ci` must pass before you push. The tests need `git` but not Claude, and cost no tokens;
[docs/development.md](docs/development.md) explains how they work and where the code lives.

## Commits

Use [conventional commits](https://www.conventionalcommits.org/) with a lowercase subject:
`feat: add prf version`, `fix: keep the branch when a gate fails`, `docs: ...`, `refactor: ...`,
`test: ...`, `ci: ...`, `chore: ...`. Release notes are built from the `feat` and `fix` commits.

## Pull requests

- Keep each PR small and focused on one change; split unrelated fixes into their own PRs.
- Say what changed and how you tested it (the PR template has a checklist).
- A PR needs a green `ci` check and the owner's review before it merges.

For a security problem, don't open an issue or PR: see [SECURITY.md](SECURITY.md).
