import os
import json
import datetime
from zoneinfo import ZoneInfo

import discord
from discord.ext import commands, tasks

from bot.utils.alerts import alert
from bot.officer.data import EventStore

STATE_FILE = "data/reminders.json"
REMINDER_TZ = ZoneInfo(os.getenv('REMINDER_TZ', 'America/New_York'))
REMINDER_HOUR = int(os.getenv('REMINDER_HOUR', '9'))  # local time, day of the event
KEEP_DAYS = 30  # how long sent reminders are remembered

# Which role each kind of event pings, matched on words in the event name.
# Events that match nothing (GBMs, TechX) are announced without a ping.
# Officers can override either choice per event from its card (/officer event).
ROLE_KEYWORDS = [
    ('workshops', 'REMINDER_ROLE_WORKSHOPS', ('workshop', 'intro to', 'scripting', 'tutorial', 'guide to', 'how to', 'hands-on')),
    ('socials', 'REMINDER_ROLE_SOCIALS', ('social', 'game night', 'picnic', 'party', 'mixer', 'bbq')),
    ('prodev', 'REMINDER_ROLE_PRODEV', ('linkedin', 'career', 'resume', 'interview', 'panel')),
]
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
    """Posts a same-day reminder for each Discord scheduled event.

    At REMINDER_HOUR local time on the day of an event, it posts once in
    REMINDER_CHANNEL_ID, pinging the event's role. Sent reminders are kept in
    STATE_FILE so restarts never post twice.
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

    def override_for(self, event):
        """The officer's reminder settings for a Discord event, via its calendar event."""
        sync = self.bot.get_cog("CalendarSync")
        gid = next((g for g, d in sync.state["event_map"].items() if d == event.id), None) if sync else None
        return EventStore().get(gid).get('reminder') if gid else None

    # ---------- what is due ----------

    def due_today(self, guild):
        """Today's not-yet-started events, earliest first."""
        now = datetime.datetime.now(REMINDER_TZ)
        events = []
        for event in guild.scheduled_events:
            if event.status != discord.EventStatus.scheduled:
                continue
            start = event.start_time.astimezone(REMINDER_TZ)
            if start.date() == now.date() and start > now:
                events.append(event)
        return sorted(events, key=lambda e: e.start_time)

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
        if datetime.datetime.now(REMINDER_TZ).hour < REMINDER_HOUR:
            return
        guild = self.bot.get_guild(self.guild_id)
        channel = guild.get_channel(self.channel_id) if guild else None
        if not guild or not channel:
            print("Error: Could not find guild or reminder channel.")
            return

        sent = self.state["sent"]
        for event in self.due_today(guild):
            key = str(event.id)
            if key in sent:
                continue
            override = self.override_for(event)
            send, why = should_send(event, override)
            if not send:
                print(f"Not posting reminder for {event.name}: {why}")
                continue
            content, allowed = build_message(event, guild.id, override)
            await channel.send(content, allowed_mentions=allowed)
            sent[key] = event.start_time.isoformat()
            self.save_state()
            print(f"Posted reminder: {event.name}")

        cutoff = discord.utils.utcnow() - datetime.timedelta(days=KEEP_DAYS)
        for key, when in list(sent.items()):
            if datetime.datetime.fromisoformat(when) < cutoff:
                del sent[key]
        self.save_state()

async def setup(bot):
    await bot.add_cog(EventReminders(bot))
