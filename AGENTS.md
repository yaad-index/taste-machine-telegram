# Working on taste-machine-telegram

This file is for people and agents changing taste-machine-telegram. `README.md` is for people using it.

## What this is

A Go service: a Telegram frontend for the taste-machine engine, which it imports as a library pinned to a released version. Design decisions are numbered Architecture Decision Records in `adr/`; read the ADR governing an area before changing that area.

## Packages

- `config`: the configuration, read from the environment. Errors name the variable and never print its value.
- `access`: who may use the bot: admins from the configuration, and users and group chats on an allowlist file with one-time invites.
- `userfiles`: each user's shelf and taste files. A compile writes a new version directory and a `current` symlink is swapped to it by rename, so readers never see a mix of two versions.
- `compile`: runs the engine's `compile` command for a linked account, one compile at a time through a queue. Source-specific settings, such as a source's API key, stay with that command: it inherits this process's environment, less the bot token.
- `flow`: the engine's question flow as chat screens (text and buttons) and what a tap on a button leads to. Button data carries a session id and a step, so a button of an answered question or an ended session is recognised.
- `bot`: command routing, the access rules, the link commands and pick sessions, on its own `Update` type and a `Sender`, so it is tested without the Telegram client.
- `cmd/taste-machine-telegram`: `serve` wires the Telegram client (long polling) to `bot`; `version` prints the version.

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
