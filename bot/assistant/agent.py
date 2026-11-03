import os
import re
import asyncio
import datetime
from dataclasses import dataclass, field

import anthropic
import discord

from bot.officer import new_event, review
from bot.officer.data import EventStore, SKIPPED, add_deadline
from bot.officer.sources import TZ
from bot.officer.steps import event_steps, owner_of, problems, no_room
from bot.utils import event_text
from bot.utils.gcal_client import patch_event
from bot.utils.event_text import same_place
from bot.assistant.tools_files import FileTools, FILE_TOOLS

# /officer ask: Claude answers an officer's question from the knowledge cache,
# the event data, Google Drive and Notion, and can line up changes to events,
# Drive and Notion (tools_files.py). Read tools run right away. Change tools only
# queue a Change: nothing happens until the officer who asked clicks Confirm
# under the reply (bot/cogs/assistant.py). Public posts still go through officer review.

MODEL = "claude-sonnet-5-5"
MAX_TURNS = 10
STEPS = {"description": "Description", "flyer": "Flyer", "announcement": "Announcement",
         "checkin": "Check-in", "slides": "Slides", "recap": "Recap post"}
store = EventStore()

SYSTEM = """You are the officer assistant for the IEEE Computer Society student chapter at UCF, inside its Discord server.
Officers ask you questions about the chapter, or ask you to change things: events, the chapter's Google Drive (ieeecsucf@gmail.com: files, folders, Docs, Sheets) and its Notion (pages and databases).

Questions:
- Search the knowledge cache before answering anything factual. It holds the server's Discord messages, the chapter's Drive documents and its Notion pages. Search again with other words if the first results miss. Use the Drive and Notion tools to read a file or page in full.
- Answer from what you found and link the messages or docs you used. If you can't find it, say so; don't guess.

Changes:
- Find what you're changing first (list_events, search_drive, search_notion, find_discord_files), then call the change tool. Ask instead of guessing when the target or the change is unclear, e.g. when two folders could match.
- To file something someone posted, find the message with find_discord_files, then save_discord_file_to_drive or add_discord_file_to_notion.
- Change tools don't change anything themselves. They queue the change, and it only happens when the officer who asked clicks Confirm under your reply. So describe what will change; never say it's done.
- Deleting only moves things to Drive's or Notion's trash, where they can be restored.
- "Set the room" means the room is confirmed, unless they say it's only requested.
- You can't post publicly. Announcements go to other officers for review with send_announcement_for_review.

Your reply is posted as a Discord message in an officer channel. Keep it short and plain, use Discord markdown, and write people's names instead of @mentions."""

TOOLS = [
    {"name": "search", "description": "Full-text search of the knowledge cache: Discord messages, Drive documents and Notion pages. Returns the best matches with links.",
     "input_schema": {"type": "object", "properties": {
         "query": {"type": "string", "description": "Words to search for"},
         "source": {"type": "string", "enum": ["any", "discord", "drive", "notion"]},
         "limit": {"type": "integer", "minimum": 1, "maximum": 20}}, "required": ["query"]}},
    {"name": "read_discord_context", "description": "The messages around one Discord message, to see the whole conversation. Pass a message link from search results.",
     "input_schema": {"type": "object", "properties": {"url": {"type": "string"}}, "required": ["url"]}},
    {"name": "list_events", "description": "Events on the chapter calendar from a week ago onward, with id, date, time, room, owner and how many steps are open.",
     "input_schema": {"type": "object", "properties": {"name_contains": {"type": "string"}}}},
    {"name": "event_details", "description": "One event's full checklist (every step's state), description, room request and listing links.",
     "input_schema": {"type": "object", "properties": {"event_id": {"type": "string"}}, "required": ["event_id"]}},
    {"name": "find_member", "description": "Look up server members by name, to get the user id for set_owner.",
     "input_schema": {"type": "object", "properties": {"name": {"type": "string"}}, "required": ["name"]}},
    {"name": "set_room", "description": "Queue a room change. 'confirmed' also updates the calendar event's location (and Discord follows).",
     "input_schema": {"type": "object", "properties": {
         "event_id": {"type": "string"}, "room": {"type": "string", "description": "e.g. ENG2 205"},
         "status": {"type": "string", "enum": ["confirmed", "requested"]}}, "required": ["event_id", "room", "status"]}},
    {"name": "edit_event", "description": "Queue changes to an event's title, date, time or description in Google Calendar (Discord follows). Leave out what doesn't change.",
     "input_schema": {"type": "object", "properties": {
         "event_id": {"type": "string"}, "title": {"type": "string"},
         "date": {"type": "string", "description": "MM/DD/YYYY"},
         "time": {"type": "string", "description": "e.g. 6:30 PM-8:30 PM"},
         "description": {"type": "string"}}, "required": ["event_id"]}},
    {"name": "set_owner", "description": "Queue a new owner for an event (who gets its nudges).",
     "input_schema": {"type": "object", "properties": {"event_id": {"type": "string"}, "user_id": {"type": "string"}},
                      "required": ["event_id", "user_id"]}},
    {"name": "mark_step", "description": "Queue marking one of an event's steps as done or not needed, or clearing the mark.",
     "input_schema": {"type": "object", "properties": {
         "event_id": {"type": "string"}, "step": {"type": "string", "enum": list(STEPS)},
         "value": {"type": "string", "enum": ["done", "not needed", "clear"]}}, "required": ["event_id", "step", "value"]}},
    {"name": "add_deadline", "description": "Queue a chapter deadline (shows in /officer chapter and the officer digest).",
     "input_schema": {"type": "object", "properties": {"date": {"type": "string", "description": "YYYY-MM-DD"},
                                                       "what": {"type": "string"}}, "required": ["date", "what"]}},
    {"name": "send_announcement_for_review", "description": "Queue sending an #announcements post to other officers for review. It posts only after an officer other than the asker approves it. Leave out event_id for a general announcement.",
     "input_schema": {"type": "object", "properties": {"text": {"type": "string"}, "event_id": {"type": "string"}},
                      "required": ["text"]}},
]
TOOLS += FILE_TOOLS
TOOLS[-1] = {**TOOLS[-1], "cache_control": {"type": "ephemeral"}}  # tools and system prompt never change: cache them


@dataclass
class Change:
    summary: str
    apply: object  # async callable returning a line of text about what happened


@dataclass
class Ask(FileTools):
    bot: object
    guild: object
    asker: object
    knowledge: object
    channel: object = None
    changes: list = field(default_factory=list)
    usage: dict = field(default_factory=lambda: {"input": 0, "cached": 0, "cache_write": 0, "output": 0})

    # ---------- the loop ----------

    async def run(self, question):
        """Claude's reply as text (changes, if any, are in self.changes)."""
        now = datetime.datetime.now(TZ)
        messages = [{"role": "user", "content":
                     f"Today is {now:%A, %B %-d, %Y, %-I:%M %p} Eastern. Asked by {self.asker.display_name}.\n\n{question}"}]
        client = anthropic.AsyncAnthropic()
        for _ in range(MAX_TURNS):
            try:
                response = await client.beta.messages.create(
                    model=MODEL, max_tokens=16000, output_config={"effort": "low"},
                    betas=["server-side-fallback-2026-07-01"], fallbacks="default",
                    system=SYSTEM, tools=TOOLS, messages=messages, cache_control={"type": "ephemeral"})
            except anthropic.RateLimitError:
                return "Claude is busy right now. Try again in a minute."
            except anthropic.APIStatusError as e:
                return f"Claude returned an error ({e.status_code})."
            except anthropic.APIConnectionError:
                return "Couldn't reach Claude."
            self.count(response.usage)
            if response.stop_reason == "refusal":
                return "Claude declined to answer that one."
            if response.stop_reason != "tool_use":
                return "".join(b.text for b in response.content if b.type == "text").strip() or "(no answer)"
            messages.append({"role": "assistant", "content": response.content})
            results = []
            for block in response.content:
                if block.type == "tool_use":
                    try:
                        result, error = await self.tool(block.name, block.input), False
                    except Exception as e:  # a tool failing shouldn't end the answer
                        result, error = f"Error: {e}", True
                    results.append({"type": "tool_result", "tool_use_id": block.id, "content": result, "is_error": error})
            messages.append({"role": "user", "content": results})
        return "That took too many steps; try asking more specifically."

    def count(self, u):
        self.usage["input"] += u.input_tokens
        self.usage["cached"] += u.cache_read_input_tokens or 0
        self.usage["cache_write"] += u.cache_creation_input_tokens or 0
        self.usage["output"] += u.output_tokens

    async def tool(self, name, args):
        handler = getattr(self, f"t_{name}", None)
        if handler is None:
            raise ValueError(f"no tool named {name}")
        return await handler(**args)

    # ---------- reading ----------

    async def t_search(self, query, source="any", limit=8):
        hits = await asyncio.to_thread(self.knowledge.search, query, source, min(int(limit), 20))
        if not hits:
            return "No matches."
        return "\n\n".join(f"[{h['source']}] {h['place']} · {h['author'] or '-'} · {h['ts'][:10]}\n{h['url']}\n{h['text'][:800]}"
                           for h in hits)

    async def t_read_discord_context(self, url):
        found = re.search(r"channels/\d+/(\d+)/(\d+)", url)
        if not found:
            return "That isn't a Discord message link."
        channel = self.bot.get_channel(int(found[1])) or await self.bot.fetch_channel(int(found[1]))
        lines = [f"{m.created_at.astimezone(TZ):%b %-d %-I:%M %p} {m.author.display_name}: {m.content}"
                 async for m in channel.history(around=discord.Object(int(found[2])), limit=21)]
        return "\n".join(reversed(lines)) or "No messages there."

    async def snapshot(self):
        return await self.bot.get_cog("Officer").cache.get()

    async def event(self, event_id):
        event = (await self.snapshot()).event(event_id)
        if event is None:
            raise ValueError(f"no event with id {event_id}; use list_events")
        return event

    async def t_list_events(self, name_contains=""):
        snap = await self.snapshot()
        since = datetime.datetime.now(TZ) - datetime.timedelta(days=7)
        rows = []
        for e in sorted(snap.events, key=lambda e: e['start']):
            if e['start'] < since or name_contains.casefold() not in e['name'].casefold():
                continue
            owner, _ = owner_of(e, snap.store.get(e['id'], {}))
            rows.append(f"{e['id']} | {e['name']} | {e['start']:%a %b %-d %-I:%M %p}-{e['end']:%-I:%M %p} | "
                        f"room: {e['location'] or 'TBA'} | owner: {self.member_name(owner)} | "
                        f"{len(problems(event_steps(e, snap)))} open steps")
        return "\n".join(rows) or "No matching events."

    async def t_event_details(self, event_id):
        snap = await self.snapshot()
        e = await self.event(event_id)
        record = snap.store.get(event_id, {})
        steps = "\n".join(f"{s.state} {s.phase} · {s.label}" + (f": {s.detail}" if s.detail else "")
                          for s in event_steps(e, snap))
        return (f"{e['name']} | {e['start']:%A %b %-d, %-I:%M %p}-{e['end']:%-I:%M %p} | room: {e['location'] or 'TBA'}\n"
                f"Description: {event_text.plain_description(e) or '(none)'}\n"
                f"Room request: {record.get('room', 'none')}\nLinks: {record.get('links', {})}\n\nSteps:\n{steps}")

    async def t_find_member(self, name):
        members = await self.guild.query_members(query=name, limit=5)
        return "\n".join(f"{m.id} | {m.display_name} ({m.name})" for m in members) or "Nobody by that name."

    def member_name(self, user_id):
        member = self.guild.get_member(int(user_id)) if user_id else None
        return member.display_name if member else (str(user_id) if user_id else "none")

    # ---------- changes (queued until Confirm) ----------

    def queue(self, summary, apply):
        self.changes.append(Change(summary, apply))
        return f"Queued: {summary}. It happens only when {self.asker.display_name} clicks Confirm."

    async def t_set_room(self, event_id, room, status):
        e = await self.event(event_id)
        room = " ".join(room.split())
        async def apply():
            store.set_room(event_id, status, room, self.asker.id)
            if status == "confirmed" and (no_room(e['location']) or not same_place(room, e['location'])):
                await asyncio.to_thread(patch_event, os.getenv('GOOGLE_CALENDAR_ID'), event_id, {'location': room})
                self.sync_soon()
                return f"Room for {e['name']} confirmed as {room}; the calendar is updated and Discord follows shortly."
            return f"Room for {e['name']} marked {status}: {room}."
        return self.queue(f"room for **{e['name']}** ({e['start']:%a %b %-d}) → **{room}** ({status})", apply)

    async def t_edit_event(self, event_id, title=None, date=None, time=None, description=None):
        e = await self.event(event_id)
        body, parts = {}, []
        if title and title.strip() != e['name']:
            body['summary'] = " ".join(title.split())
            parts.append(f"title → **{body['summary']}**")
        if date or time:
            start, end = new_event.parse_form(date or f"{e['start']:%m/%d/%Y}",
                                              time or f"{e['start']:%-I:%M %p}-{e['end']:%-I:%M %p}",
                                              datetime.datetime.now(TZ))
            body['start'] = {"dateTime": start.isoformat(), "timeZone": str(TZ)}
            body['end'] = {"dateTime": end.isoformat(), "timeZone": str(TZ)}
            parts.append(f"time → **{start:%a %b %-d, %-I:%M %p}-{end:%-I:%M %p}**")
        if description is not None and description.strip() != e['description']:
            body['description'] = description.strip()
            parts.append("description")
        if not body:
            return "Nothing would change."
        async def apply():
            await asyncio.to_thread(patch_event, os.getenv('GOOGLE_CALENDAR_ID'), event_id, body)
            self.sync_soon()
            return f"Updated {e['name']}: {', '.join(parts)}. Discord follows shortly."
        return self.queue(f"**{e['name']}**: {', '.join(parts)}", apply)

    async def t_set_owner(self, event_id, user_id):
        e = await self.event(event_id)
        uid = int(re.sub(r"\D", "", str(user_id)))
        name = self.member_name(uid)
        async def apply():
            store.set_owner(event_id, uid)
            return f"{name} now owns {e['name']}."
        return self.queue(f"owner of **{e['name']}** → **{name}**", apply)

    async def t_mark_step(self, event_id, step, value):
        e = await self.event(event_id)
        stored = {"done": "done", "not needed": SKIPPED, "clear": None}[value]
        async def apply():
            store.mark(event_id, step, stored)
            return f"{STEPS[step]} for {e['name']}: {value}."
        return self.queue(f"**{e['name']}**: mark {STEPS[step]} as {value}", apply)

    async def t_add_deadline(self, date, what):
        day = datetime.date.fromisoformat(date)
        async def apply():
            add_deadline(day, what.strip())
            return f"Added deadline {day:%a %b %-d, %Y}: {what.strip()}."
        return self.queue(f"deadline **{day:%a %b %-d, %Y}**: {what.strip()}", apply)

    async def t_send_announcement_for_review(self, text, event_id=None):
        from bot.officer.views import review_buttons
        if event_id:
            e = await self.event(event_id)
            kind, target, label = "announcement", e, f"announcement for **{e['name']}**"
        else:
            kind, target, label = "general", {"id": f"general_{discord.utils.time_snowflake(discord.utils.utcnow())}",
                                              "name": "General announcement"}, "general announcement"
        async def apply():
            flyer = None
            sync = self.bot.get_cog("CalendarSync")
            if kind == "announcement" and sync and target.get('flyer'):
                flyer = await sync.download_flyer(target['flyer'])
            result, message = await review.open_review(self.bot, target, kind, text, self.asker.id,
                                                       view_for=review_buttons, flyer=flyer)
            return f"Sent the {label.replace('**', '')} for review: {message.jump_url}" if result else message
        return self.queue(f"send the {label} for review:\n> " + text[:300].replace("\n", "\n> ")
                          + ("…" if len(text) > 300 else ""), apply)

    def sync_soon(self):
        sync = self.bot.get_cog("CalendarSync")
        if sync:
            sync.sync_soon(5)
