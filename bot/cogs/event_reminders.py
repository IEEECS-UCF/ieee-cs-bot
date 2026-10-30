import os
import json
import datetime
import traceback
from zoneinfo import ZoneInfo

import discord
from discord.ext import commands, tasks

STATE_FILE = "data/reminders.json"
REMINDER_TZ = ZoneInfo(os.getenv('REMINDER_TZ', 'America/New_York'))
REMINDER_HOUR = int(os.getenv('REMINDER_HOUR', '9'))  # local time, day of the event
KEEP_DAYS = 30  # how long sent reminders are remembered

# Which role each kind of event pings, matched on words in the event name.
# Events that match nothing (GBMs, TechX) are announced without a ping.
ROLE_KEYWORDS = [
    ('REMINDER_ROLE_WORKSHOPS', ('workshop', 'intro to', 'scripting', 'tutorial')),
    ('REMINDER_ROLE_SOCIALS', ('social', 'game night', 'picnic', 'party', 'mixer', 'bbq')),
    ('REMINDER_ROLE_PRODEV', ('linkedin', 'career', 'resume', 'interview', 'panel')),
]


def is_unconfirmed(event):
    """No reminder for events without a real room or marked tentative."""
    location = (event.location or '').strip().casefold()
    return location in ('', 'tba', 'tbd') or 'tentative' in event.name.casefold()

def role_for(name):
    lowered = name.casefold()
    for env_key, words in ROLE_KEYWORDS:
        role_id = os.getenv(env_key)
        if role_id and any(w in lowered for w in words):
            return int(role_id)
    return None

def format_time(dt):
    return dt.astimezone(REMINDER_TZ).strftime('%-I:%M %p')

def build_message(event, guild_id):
    role_id = role_for(event.name)
    mention = f"<@&{role_id}> " if role_id else "Hey everyone! "
    return (
        f"{mention}**{event.name}** is TODAY! ( ´ ▽ ` )ﾉ\n"
        f"🕡 {format_time(event.start_time)}  ·  📍 {event.location}\n"
        f"https://discord.com/events/{guild_id}/{event.id}\n"
        f"(Please mark interested if you're coming!)"
    ), role_id


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
        self.state = self.load_state()
        if self.channel_id:
            self.send_reminders.start()
        else:
            print("Event reminders disabled: REMINDER_CHANNEL_ID not set.")

    def cog_unload(self):
        self.send_reminders.cancel()

    # ---------- state ----------

    def load_state(self):
        if os.path.exists(STATE_FILE):
            try:
                with open(STATE_FILE, "r") as f:
                    return json.load(f)
            except (json.JSONDecodeError, OSError) as e:
                print(f"Reminder state unreadable ({e}); starting fresh.")
        return {"sent": {}}

    def save_state(self):
        tmp = STATE_FILE + ".tmp"
        with open(tmp, "w") as f:
            json.dump(self.state, f, indent=2)
        os.replace(tmp, STATE_FILE)

    # ---------- what is due ----------

    def due_today(self, guild):
        """Today's not-yet-started, confirmed events, earliest first."""
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
        except Exception:
            print("Event reminders failed:")
            traceback.print_exc()

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
            if is_unconfirmed(event):
                print(f"Skipping reminder for unconfirmed event: {event.name}")
                continue
            content, role_id = build_message(event, guild.id)
            allowed = discord.AllowedMentions(
                everyone=False, users=False,
                roles=[discord.Object(role_id)] if role_id else False,
            )
            await channel.send(content, allowed_mentions=allowed)
            sent[key] = event.start_time.isoformat()
            self.save_state()
            print(f"Posted reminder: {event.name}")

        cutoff = discord.utils.utcnow() - datetime.timedelta(days=KEEP_DAYS)
        for key, when in list(sent.items()):
            if datetime.datetime.fromisoformat(when) < cutoff:
                del sent[key]
        self.save_state()

    # ---------- preview ----------

    def preview_lines(self, guild):
        """What will post in the next 7 days, for /officer reminders."""
        today = datetime.datetime.now(REMINDER_TZ).date()
        lines = []
        for event in sorted(guild.scheduled_events, key=lambda e: e.start_time):
            day = event.start_time.astimezone(REMINDER_TZ).date()
            if event.status != discord.EventStatus.scheduled or not 0 <= (day - today).days <= 7:
                continue
            if is_unconfirmed(event):
                status = "skipped (room TBA or tentative)"
            elif str(event.id) in self.state["sent"]:
                status = "already posted"
            else:
                status = f"posts {day:%a} at {REMINDER_HOUR}:00"
            content, _ = build_message(event, guild.id)
            quoted = "\n".join(f"> {line}" for line in content.splitlines())
            lines.append(f"**{event.name}** · {status}\n{quoted}")
        return lines or ["No events in the next 7 days."]


async def setup(bot):
    await bot.add_cog(EventReminders(bot))
