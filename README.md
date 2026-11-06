# IEEE CS UCF Discord bot

The chapter's Discord bot, written in Go. It mirrors Google Calendar to Discord
events, keeps officers on top of each event (`/officer events`, `/officer event`,
nudges, the digest, reviews for announcements and day-of reminders), runs the
volunteer board, the job feed, `/events` for members and `/member` verification, and answers
`/officer ask` with Claude, from a knowledge cache of Discord, Google Drive and
Notion.

## Layout

- `cmd/ieee-bot`: the program.
- `internal/cogs/*`: each feature, set up from `main.go`.
- `internal/officer`, `internal/assistant`, `internal/store` and the rest: the logic and
  clients they share.
- `config/deadlines.json`: chapter deadlines kept in the repo.
- `internal/aidraft/announcement_examples.json`: the announcements AI drafts copy the style of.
- `apps_script/`: the check-in form's Apps Script.
- `scripts/drive_login.py`: the one-time Google sign-in for Drive.
- `GO_PORT.md`: how the bot was ported from the original Python version.

## Running and testing

Go 1.27 or newer:

```bash
go test -race ./...
go build -o ieee-bot ./cmd/ieee-bot
```

The bot reads its settings from the environment (or a `.env` file) and keeps
state in `data/` and `cache/` under its working directory, so run it from the
repo root. `COMMAND_GUILD_ID` registers the slash commands on one server only,
which shows them at once (handy for a test server).

## Deploying

Pushing `main` runs `.github/workflows/deploy.yml`: it tests, builds the Linux
binary and sends it on stdin to `~/deploy/deploy.sh cs` on the server. That
script updates the checkout in `~/ieee-cs-bot` (for `config/` and the docs),
installs the binary at `~/ieee-bot-go/ieee-bot`, keeps the previous one as
`ieee-bot.prev`, restarts the `ieee-bot-go` service, and puts the previous
binary back if the new one doesn't log in within a minute.

The service runs in `~/ieee-cs-bot` with the env file `/etc/ieee-bot.env`.
