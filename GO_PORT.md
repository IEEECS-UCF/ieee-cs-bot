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
| `bot/assistant/{agent,knowledge,notion,drive,tools_files}.py` | `internal/assistant` |
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

## Status

Everything in `bot/` is ported, including the Drive and Notion tools for
`/officer ask` from main @ fe7df9d (merged into this branch):

- Calendar sync (both directions, weekly summary, flyer covers, the
  deletion cap), day-of reminders with review, `/officer events|event|chapter|announce|ask`,
  the board, cards, pickers and modals, reviews and emergency posts, the digest,
  nudge forum posts with edited status messages, monthly metrics, tabling
  reminders, `/member verify|status` with officer review, the job feed and all
  its sources, the volunteer board, `/socials` and `/info`.
- `/officer ask`: Claude Sonnet 5.5 at low effort with every tool, confirm-before-change,
  the FTS5 knowledge cache of Discord, Drive and Notion.
- Tests: about 220 Go tests, ported case by case from `tests/*.py` plus new
  ones (regex rewrites checked against Python's results, JSON round trips,
  a startup smoke test that sets up every cog and checks the slash commands
  match the Python bot's). `go test -race ./...` passes. No test touches the network.

Not done yet:

- **Never run against real Discord, Google, Claude or Notion.** Everything is
  tested through fakes. The first run on a test guild is the real test,
  especially modals, editing ephemeral messages later, scheduled-event cover
  images, member chunking and the forum posts.
- **CPU and memory not measured.** The reason for the port (a history re-read
  pinning a core at about 145 MB) needs a side-by-side run to confirm.
- `scripts/drive_login.py` stays Python. It's a one-time sign-in, and the Go bot
  reads the token file it writes.

Differences worth knowing (details in the commit messages):

- Expired or pre-restart ephemeral menus answer "This menu has expired" instead
  of Discord's "interaction failed".
- `tasks.loop` stops for good after an uncaught exception; Go loops alert and keep going.
- JSON files come out with keys in sorted order and non-ASCII written as UTF-8
  (Python wrote insertion order and `\u` escapes). Same data; either bot reads
  the other's files. Unknown keys in records are kept.
- Regexes: Go's RE2 has no lookaround, so the few that used it are hand-written
  functions, and casefold is `strings.ToLower`.
- Confirming a room also calls a calendar sync 5 s later, so the new location
  reaches Discord sooner.
- Dates in the new-event form are read by a hand-written parser instead of
  dateutil. It covers the formats officers type (11/20, 2026-11-20, Nov 20, Fri Nov 20th...).

## Running it side by side with the Python bot

Never run both bots on the production token at the same time: both would
answer every interaction and run every loop, so posts and calendar writes would
double up. Test with a second Discord application on a test guild.

1. In the Discord Developer Portal, create a new application (for example "IEEE CS
   Bot (Go test)"). Turn on the Message Content and Server Members intents, and
   invite it to a test guild with the same permissions as the real bot.
2. Make a test copy of the guild's setup: an events channel, a nudge forum,
   an announcements channel, a reminders channel, an alert channel, officer roles.
3. **Use a separate Google Calendar.** Sync is two-way, and deleting a Discord
   event deletes the calendar event. Create a test calendar and share it with
   the service account (writer), or share the real one read-only. With only
   reader access, the bot never writes back.
4. Make a separate directory with a test env file. Don't edit `/etc/ieee-bot.env`.
   ```bash
   mkdir -p ~/go-test/{data,cache,credentials,bot}
   cp bot/deadlines.json ~/go-test/bot/
   cp credentials/google_service.json ~/go-test/credentials/
   ```
   The test `~/go-test/.env` uses the same variable names with test values:
   `DISCORD_TOKEN` (test app), `GUILD_ID` plus every `*_CHANNEL_ID` / `*_ROLE_ID`
   from the test guild, `OFFICER_ROLE_IDS`, `GOOGLE_CALENDAR_ID` (test
   calendar), `MEMBERS_INTENT=1`, and `COMMAND_GUILD_ID=<test guild>` so
   commands show up at once. Leave `CHECKIN_URL`, `NOTION_TOKEN` and
   `credentials/drive_user.json` out at first: those reach the chapter's real
   forms, Notion and Drive. Add them when you want to test those features, and
   only confirm changes you'd be fine with for real.
5. Build and run it from that directory:
   ```bash
   go build -o ~/go-test/ieee-bot-go ./cmd/ieee-bot && cd ~/go-test && ./ieee-bot-go
   ```
6. To compare with the Python bot, point the Python bot at the same test env
   in another directory, and run one bot at a time against the test guild.
   To compare resource use, run each through a full knowledge-cache back-fill
   (delete `cache/knowledge.db`) and watch `ps -o rss,pcpu`.

## In production (since Nov 3, 2026)

The Go bot runs on the server as systemd `ieee-bot-go`, from
`/home/ubuntu/ieee-bot-go/ieee-bot`, in the Python bot's directory with its env
file, `data/` and `cache/`. The Python unit `ieee-bot` is stopped and disabled,
and the two units conflict, so only one runs.

Pushing `main` runs `deploy.yml`: it tests both bots, builds the Go binary and
sends it to `~/deploy/deploy.sh cs` on stdin. While `~/deploy/cs-runs-go`
exists, that script updates the Python code (for a quick revert), installs the
binary, keeps the previous one as `ieee-bot.prev`, and puts it back if the new
one doesn't log in within a minute.

To go back to the Python bot:

```bash
rm ~/deploy/cs-runs-go; sudo systemctl disable --now ieee-bot-go; sudo systemctl enable --now ieee-bot
```

Either bot reads the other's `data/` files.
