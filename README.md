# taste-machine-telegram

A Telegram frontend for [taste-machine](https://github.com/yaad-index/taste-machine): pick from a shelf in a chat, alone or as a group.

Work in progress; the design is recorded in `adr/`.

## Development

Requires only the Go version in `go.mod`; the formatters and the linter are built from `tools/go.mod`.

```
make check   # vet, build, race tests, formatting, lint, tidiness: the same as CI
```

See `AGENTS.md` for working on the code.
