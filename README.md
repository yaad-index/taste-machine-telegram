# taste-machine-telegram

A Telegram frontend for [taste-machine](https://github.com/yaad-index/taste-machine): pick from a shelf in a chat, alone or as a group.

Work in progress; the design is recorded in `adr/`.

## Running

The image holds the bot and the engine's `compile` command, built from the same engine version. It runs as a non-root user and keeps everything under `/data`.

| Variable | Required | Meaning |
|---|---|---|
| `TASTE_MACHINE_TELEGRAM_TOKEN` | yes | The bot token. |
| `TASTE_MACHINE_TELEGRAM_ADMINS` | yes | Admin Telegram user ids, comma-separated. Admins can `/invite`, `/revoke`, `/allow` and `/disallow`. |
| `TASTE_MACHINE_TELEGRAM_DATA_DIR` | no | The data directory (the image sets `/data`). |
| `TASTE_MACHINE_TELEGRAM_HEALTH_ADDR` | no | Where the health endpoint listens (default `:8080`). `GET /healthz` answers 200 while the bot polls successfully, 503 otherwise. |
| `TASTE_MACHINE_TELEGRAM_COMPILE` | no | The engine's command (the image has it on `PATH`). |
| `TASTE_MACHINE_TELEGRAM_API_URL` | no | A Bot API server other than Telegram's own. |

A source's API key is read by the engine's `compile` command, under the variable that command documents. The bot passes its environment on to that command without the bot token, and never reads the key itself.

A minimal compose file:

```yaml
services:
  bot:
    image: ghcr.io/yaad-index/taste-machine-telegram:latest
    restart: unless-stopped
    environment:
      TASTE_MACHINE_TELEGRAM_TOKEN: ${BOT_TOKEN:?}
      TASTE_MACHINE_TELEGRAM_ADMINS: ${ADMIN_IDS:?}
      TASTE_MACHINE_BGG_API_KEY: ${SOURCE_API_KEY:?}
    volumes:
      - data:/data

volumes:
  data:
```

The image's health check runs `taste-machine-telegram health`, so `docker ps` shows the bot's state without publishing a port.

## Development

Requires only the Go version in `go.mod`; the formatters and the linter are built from `tools/go.mod`.

```
make check   # vet, build, race tests, formatting, lint, tidiness: the same as CI
```

See `AGENTS.md` for working on the code.
