# Go port (experimental, branch `go-port`)

A port of the bot to Go (discordgo, anthropic-sdk-go, google.golang.org/api)
meant as a drop-in replacement: same `/etc/ieee-bot.env` variables, same
`data/*.json` files, same `cache/knowledge.db`, same slash commands and the same
button custom IDs, so messages the Python bot already posted keep working.

The Python bot stays in `bot/` and is still what `main` deploys. Nothing here
changes `.github/workflows/deploy.yml`.

## Layout: Python module → Go package

| Python | Go |
| --- | --- |
| `bot/main.py` | `cmd/ieee-bot` (wiring, command sync, error handler) |
| discord.py `commands.Bot`, `app_commands`, `ui.View`, `ui.Modal`, `DynamicItem`, `tasks.loop` | `internal/botkit` |
| `bot/utils/alerts.py` | `internal/botkit/alerts.go` |
| `bot/utils/formatting.py` | `internal/botkit/embed.go` |
| `bot/officer/data.py` (`load_json`, `save_json`, `EventStore`, deadlines, `migrate`) | `internal/store` |
| `bot/utils/gcal_client.py` | `internal/gcal` |
| `bot/utils/event_text.py` | `internal/eventtext` |
| `bot/utils/checkin_client.py` | `internal/checkin` |
| `bot/utils/knightconnect_client.py` | `internal/knightconnect` |
| `bot/utils/vtools_client.py` | `internal/vtools` |
| `bot/utils/ai_draft.py` | `internal/aidraft` |
| `bot/utils/job_listings.py`, `bot/utils/job_worker.py` | `internal/jobs` |
| `bot/officer/{sources,steps,render,metrics,review,new_event,tabling,members}.py`, the rules half of `bot/cogs/event_reminders.py` | `internal/officer` |
| `bot/officer/views.py`, `bot/cogs/officer.py` | `internal/cogs/officer` |
| `bot/cogs/calendar_sync.py` | `internal/cogs/calendarsync` |
| `bot/cogs/event_reminders.py` (the loop) | `internal/cogs/reminders` |
| `bot/cogs/assistant.py` | `internal/cogs/assistant` |
| `bot/assistant/{agent,knowledge,documents}.py` | `internal/assistant` |
| `bot/cogs/members.py` | `internal/cogs/members` |
| `bot/cogs/job_feed.py` | `internal/cogs/jobfeed` |
| `bot/cogs/volunteer.py` | `internal/volunteer` (data, embed, buttons; the officer cog uses it too) |
| `bot/cogs/general.py` | `internal/cogs/general` |
| `tests/*.py` | `_test.go` files next to the code they test |

Python imports in a circle (`steps` → `event_reminders` → `steps`, `views` ↔
`review`), which Go doesn't allow. So everything the officer tools reason about
lives in one package, `internal/officer`, and only the Discord-facing parts are
split out into `internal/cogs/*`.

## Hard to match, and what the port does

- **`DynamicItem` persistent buttons** (`task:…`, `member:…`, `officer:event:…`,
  `review:…`). discordgo has no views, so `botkit.Router` matches every
  component and modal custom ID against registered regexps. The IDs are
  unchanged, so buttons the Python bot posted work after the switch.
- **Ephemeral `ui.View`s with timeouts and in-memory state** (event card,
  pickers, previews). `botkit.View` hands out custom IDs `v:<token>:<n>`, keeps
  the callbacks in memory and forgets them after the timeout (840 s, reset on
  every use, like discord.py). Pressing a button on an expired or pre-restart
  view gets a short "this menu expired" reply instead of Discord's "interaction
  failed".
- **Modals**: plain text-input modals only, same as the Python bot; `botkit`
  routes the submit back to a closure.
- **Editing an ephemeral message later** (`view.message.edit`): `botkit.Msg`
  remembers the interaction token and edits through the webhook, which is what
  discord.py does under the hood. Same 15-minute limit.
- **Concurrency**: asyncio ran one handler at a time between awaits; discordgo
  runs handlers on goroutines. Every read-modify-write of a `data/*.json` file
  goes through `store.Update`, which holds a per-file lock, and cog state has
  its own mutex.
- **Discord IDs in JSON**: snowflakes don't fit in float64, so the data files
  are decoded into typed structs (`int64`), never `map[string]any`. Records keep
  keys the Go code doesn't know about, so nothing is dropped on save.
- **`tasks.loop`**: `botkit.Loop` runs a function now and then on an interval
  once the gateway is ready, recovering and alerting on panics. (A discord.py
  loop stops for good on an unhandled exception; these keep going.)
- **Job worker subprocess**: Python ran it as a child process to give the
  memory back. Go decodes the feeds straight into small structs with a
  streaming decoder, so it runs in-process.
- **Anthropic**: `anthropic-sdk-go` beta messages with `output_config.effort`
  and the server-side fallback beta, same as the Python calls.
- **SQLite FTS5**: `modernc.org/sqlite` (pure Go, FTS5 built in), so the binary
  cross-compiles for the VM without cgo.

## Building

Go 1.27 or newer. From the repo root:

```bash
go build -o ieee-bot-go ./cmd/ieee-bot
go test ./...
```

For the VM, cross-compile (no cgo needed; use `GOARCH=arm64` if the VM is an
Ampere shape):

```bash
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags=-s -o ieee-bot-go ./cmd/ieee-bot
```

The binary reads the same environment as the Python bot (`/etc/ieee-bot.env`
or a `.env` in the working directory) and the same relative paths
(`data/`, `cache/`, `credentials/google_service.json`, `bot/deadlines.json`),
so run it from a directory laid out like the repo.

One new, optional variable: `COMMAND_GUILD_ID`. When set, slash commands are
registered on that guild only (they show up at once) instead of globally.
