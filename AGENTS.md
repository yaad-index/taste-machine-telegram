# Working on taste-machine-telegram

This file is for people and agents changing taste-machine-telegram. `README.md` is for people using it.

## What this is

A Go service: a Telegram frontend for the taste-machine engine, which it imports as a library pinned to a released version. Design decisions are numbered Architecture Decision Records in `adr/`; read the ADR governing an area before changing that area.

## Before pushing

```
make check          # vet, build, race tests, formatting, lint, tidiness: what CI runs
make fmt            # apply gofumpt and goimports, tidy every module
make dist           # cross-compile the service for every release platform into dist/
make install-hooks  # optional: run the fast subset of make check on every commit
```

Nothing needs installing besides Go. The formatters and the linter are tool dependencies in `tools/go.mod`, a separate module, and run as `go tool -modfile=tools/go.mod <tool>`. Add a new dev tool the same way:

```
go get -modfile=tools/go.mod -tool <package>@<version>
```

## Conventions

- Formatting is gofumpt, with imports grouped by goimports under the local prefix `github.com/yaad-index/taste-machine-telegram`.
- Tests use testify (`require` / `assert`) and always run with `-race`.
- Pull request titles are Conventional Commits: squash merges use the title as the commit subject, and releases are computed from it.
- Secrets (the bot token, the source API key) come only from the environment and never appear in messages, logs or test output.
