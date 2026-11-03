import os
import json
import datetime
from zoneinfo import ZoneInfo

import discord
from discord.ext import commands, tasks

from bot.utils.alerts import alert
from bot.utils.event_text import ROLE_WORDS
from bot.officer.data import EventStore
from bot.officer import review

STATE_FILE = "data/reminders.json"
REMINDER_TZ = ZoneInfo(os.getenv('REMINDER_TZ', 'America/New_York'))
REMINDER_HOUR = int(os.getenv('REMINDER_HOUR', '9'))  # local time, day of the event
REVIEW_HOUR = int(os.getenv('REMINDER_REVIEW_HOUR', '12'))  # local time, the day before: when it goes up for review
KEEP_DAYS = 30  # how long sent reminders are remembered

# Which role each kind of event pings, matched on words in the event name
# (event_text.ROLE_WORDS).
# Events that match nothing (GBMs, TechX) are announced without a ping.
# Officers can override either choice per event from its card (/officer event).
ROLE_ENV = {'workshops': 'REMINDER_ROLE_WORKSHOPS', 'socials': 'REMINDER_ROLE_SOCIALS', 'prodev': 'REMINDER_ROLE_PRODEV'}
ROLE_KEYWORDS = [(key, ROLE_ENV[key], words) for key, words in ROLE_WORDS]
PING_CHOICES = {'auto', 'none', 'everyone'} | {key for key, _, _ in ROLE_KEYWORDS}
SEND_CHOICES = {'auto', 'always', 'never'}


def is_unconfirmed(event):
    """No reminder for events without a real room or marked tentative."""
    location = (event.location or '').strip().casefold()
    return location in ('', 'tba', 'tbd') or 'tentative' in event.name.casefold()

def role_for(name):
    lowered = name.casefold()
    for _, env_key, words in ROLE_KEYWORDS:
        role_id = os.getenv(env_key)
        if role_id and any(w in lowered for w in words):
            return int(role_id)
    return None

def resolve_ping(event, override):
    """('role', id), ('everyone', None) or ('none', None) for this event."""
    ping = (override or {}).get('ping', 'auto')
    if ping == 'everyone':
        return 'everyone', None
    if ping == 'none':
        return 'none', None
    for key, env_key, _ in ROLE_KEYWORDS:
        if ping == key and os.getenv(env_key):
            return 'role', int(os.getenv(env_key))
    role_id = role_for(event.name)
    return ('role', role_id) if role_id else ('none', None)

def should_send(event, override):
    """(send?, why) for this event, honoring an officer's override."""
    send = (override or {}).get('send', 'auto')
    if send == 'always':
        return True, "set to always send"
    if send == 'never':
        return False, "set to skip"
    if is_unconfirmed(event):
        return False, "skipped (room TBA or tentative)"
    return True, ""

def format_time(dt):
    return dt.astimezone(REMINDER_TZ).strftime('%-I:%M %p')

def build_message(event, guild_id, override=None):
    """The reminder text and the mentions it is allowed to ping."""
    kind, role_id = resolve_ping(event, override)
    if kind == 'role':
        mention = f"<@&{role_id}> "
        allowed = discord.AllowedMentions(everyone=False, users=False, roles=[discord.Object(role_id)])
    elif kind == 'everyone':
        mention = "@everyone "
        allowed = discord.AllowedMentions(everyone=True, users=False, roles=False)
    else:
        mention = "Hey everyone! "
        allowed = discord.AllowedMentions.none()
    content = (
        f"{mention}**{event.name}** is TODAY! ( ´ ▽ ` )ﾉ\n"
        f"🕡 {format_time(event.start_time)}  ·  📍 {event.location or 'TBA'}\n"
        f"https://discord.com/events/{guild_id}/{event.id}\n"
        f"(Please mark interested if you're coming!)"
    )
    return content, allowed


class EventReminders(commands.Cog):
    """Posts a same-day reminder for each Discord scheduled event, once an officer approves it.

    At REVIEW_HOUR the day before, the reminder goes up for review in the
    event's post in the nudge forum (bot/officer/review.py). If an officer
    approves it, it posts once in REMINDER_CHANNEL_ID at REMINDER_HOUR on the
    day (or as soon as it's approved, until the event starts). Unapproved
    reminders never post. Sent reminders are kept in STATE_FILE so restarts
    never post twice.
    """

    def __init__(self, bot):
        self.bot = bot
        self.guild_id = int(os.getenv('GUILD_ID'))
        channel = os.getenv('REMINDER_CHANNEL_ID')
        self.channel_id = int(channel) if channel else None

        os.makedirs(os.path.dirname(STATE_FILE), exist_ok=True)
        self.state = self.load_json(STATE_FILE, {"sent": {}})
        if self.channel_id:
            self.send_reminders.start()
        else:
            print("Event reminders disabled: REMINDER_CHANNEL_ID not set.")

    def cog_unload(self):
        self.send_reminders.cancel()

    # ---------- state ----------

    @staticmethod
    def load_json(path, default):
        if os.path.exists(path):
            try:
                with open(path, "r") as f:
                    return json.load(f)
            except (json.JSONDecodeError, OSError) as e:
                print(f"{path} unreadable ({e}); starting fresh.")
        return default

    @staticmethod
    def save_json(path, data):
        tmp = path + ".tmp"
        with open(tmp, "w") as f:
            json.dump(data, f, indent=2)
        os.replace(tmp, path)

    def save_state(self):
        self.save_json(STATE_FILE, self.state)

    def calendar_id(self, event):
        """The calendar event a Discord event mirrors (None for Discord-only events)."""
        sync = self.bot.get_cog("CalendarSync")
        return next((g for g, d in sync.state["event_map"].items() if int(d) == event.id), None) if sync else None

    def override_for(self, event):
        """The officer's reminder settings for a Discord event, via its calendar event."""
        gid = self.calendar_id(event)
        return EventStore().get(gid).get('reminder') if gid else None

    # ---------- what is due ----------

    def upcoming(self, guild):
        """Scheduled events that haven't started, plus any started in the last day (to close out their review)."""
        cutoff = datetime.datetime.now(REMINDER_TZ) - datetime.timedelta(days=1)
        return sorted([e for e in guild.scheduled_events if e.start_time > cutoff and
                       e.status in (discord.EventStatus.scheduled, discord.EventStatus.active)],
                      key=lambda e: e.start_time)

    # ---------- loop ----------

    @tasks.loop(minutes=10)
    async def send_reminders(self):
        # A raised exception would stop tasks.loop for good, so catch everything
        try:
            await self.run_reminders()
        except Exception as e:
            await alert(self.bot, "Event reminders", e)

    @send_reminders.before_loop
    async def before_reminders(self):
        await self.bot.wait_until_ready()

    async def run_reminders(self):
        guild = self.bot.get_guild(self.guild_id)
        channel = guild.get_channel(self.channel_id) if guild else None
        if not guild or not channel:
            print("Error: Could not find guild or reminder channel.")
            return
        officer = self.bot.get_cog("Officer")
        snap = await officer.cache.get() if officer else None

        now = datetime.datetime.now(REMINDER_TZ)
        sent = self.state["sent"]
        for event in self.upcoming(guild):
            gid = self.calendar_id(event)
            calendar_event = snap.event(gid) if snap and gid else None
            if str(event.id) in sent or calendar_event is None:
                continue
            start = event.start_time.astimezone(REMINDER_TZ)
            review_from, post_at = review.review_window(start, REVIEW_HOUR, REMINDER_HOUR)
            when = f"{start:%a %b %-d} at {REMINDER_HOUR}:00"
            override = self.override_for(event)
            send, why = should_send(event, override)
            current = review.get(gid, "reminder")
            if not send and not current:
                continue
            text, _ = build_message(event, guild.id, override)

            if current and current.get("for") == start.isoformat() and current["status"] in (review.PENDING, review.APPROVED):
                if not send:
                    # Turned off or the room fell through after it went up for review
                    await self.close_review(gid, current, review.CANCELLED, why)
                    continue
                if current.get("editor") is None and current["text"] != text:
                    # The event changed and nobody has rewritten the bot's text: show the new text, approve again
                    current = {**current, "text": text, "status": review.PENDING, "approver": None}
                    review.put(gid, "reminder", current)
                    await review.show(self.bot, "reminder", current, when, view=self.buttons(gid))

            action = review.reminder_action(now, start, current, review_from, post_at) if send else None
            from bot.officer.steps import owner_of   # steps imports this module
            owner, _ = owner_of(calendar_event, EventStore().get(gid))
            if action == "open":
                result, why_not = await review.open_review(
                    self.bot, calendar_event, "reminder", text, None, ping=owner, when=when,
                    view_for=lambda g, k: self.buttons(g), extra={"for": start.isoformat()})
                if result is None:
                    print(f"Reminder for {event.name} not put up for review: {why_not}")
                else:
                    print(f"Reminder for {event.name} is up for review")
            elif action == "post":
                message = await channel.send(current["text"], allowed_mentions=review.mentions_in(current["text"]))
                sent[str(event.id)] = event.start_time.isoformat()
                self.save_state()
                current = {**current, "status": review.POSTED, "posted": message.jump_url}
                review.put(gid, "reminder", current)
                await review.show(self.bot, "reminder", current)
                print(f"Posted reminder: {event.name}")
            elif action == "late":
                review.put(gid, "reminder", {**current, "late_noted": True})
                thread_id, message_id = current["message"]
                try:
                    await self.bot.get_partial_messageable(int(thread_id)).send(
                        (f"<@{owner}> " if owner else "") + "The day-of reminder hasn't been approved, so it hasn't posted. "
                        "It posts as soon as an officer approves it, until the event starts.",
                        reference=discord.MessageReference(message_id=int(message_id), channel_id=int(thread_id)),
                        allowed_mentions=discord.AllowedMentions(everyone=False, roles=False,
                                                                 users=[discord.Object(owner)] if owner else False))
                except discord.HTTPException as e:
                    print(f"Could not tell the owner the reminder for {event.name} isn't approved: {e}")
            elif action == "missed":
                await self.close_review(gid, current, review.MISSED)
                print(f"Reminder for {event.name} was never approved, so it wasn't posted")

        cutoff = discord.utils.utcnow() - datetime.timedelta(days=KEEP_DAYS)
        for key, when in list(sent.items()):
            if datetime.datetime.fromisoformat(when) < cutoff:
                del sent[key]
        self.save_state()

    def buttons(self, gid):
        from bot.officer.views import review_buttons
        return review_buttons(gid, "reminder")

    async def close_review(self, gid, current, status, why=None):
        """Take the review off the table (no more buttons), saying why on the message."""
        closed = {**current, "status": status, "why": why}
        review.put(gid, "reminder", closed)
        await review.show(self.bot, "reminder", closed)

async def setup(bot):
    await bot.add_cog(EventReminders(bot))
