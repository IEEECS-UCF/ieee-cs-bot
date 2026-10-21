import os
import json
import asyncio
import datetime
import traceback
from zoneinfo import ZoneInfo

import discord
from discord.ext import commands, tasks
from dateutil import parser
from bs4 import BeautifulSoup
from bot.utils.gcal_client import fetch_upcoming_events, get_event
from bot.utils.formatting import create_base_embed, format_timestamp, IEEE_GOLD

ORG_NAME = "IEEE CS"
STATE_FILE = "data/state.json"
LOOKAHEAD_DAYS = 30
SUMMARY_DAYS = 7
# A fresh summary is posted weekly (and the old one deleted) so members get
# notified; in between, the current one is edited in place.
SUMMARY_POST_WEEKDAY = 0  # Monday
SUMMARY_POST_HOUR = 9     # in the calendar's time zone

# Discord limits for scheduled events
MAX_NAME = 100
MAX_LOCATION = 100
MAX_DESCRIPTION = 400


def normalize(text):
    """Collapse whitespace the way Discord does when it stores names/locations."""
    return " ".join((text or "").split())

def match_key(name):
    return normalize(name).casefold()

def parse_gcal_time(when, default_tz):
    tz = ZoneInfo(when.get('timeZone') or default_tz)
    if 'dateTime' in when:
        dt = parser.isoparse(when['dateTime'])
        if dt.tzinfo is None:
            dt = dt.replace(tzinfo=tz)
    else:
        # All-day event: midnight in the calendar's time zone
        dt = datetime.datetime.combine(datetime.date.fromisoformat(when['date']), datetime.time(), tz)
    return dt.astimezone(datetime.timezone.utc)

def build_payload(item, default_tz):
    """Turns a Google Calendar event into the fields of a Discord scheduled event."""
    name = normalize(item.get('summary'))[:MAX_NAME] or 'Untitled Event'

    raw_description = item.get('description') or ''
    clean_text = BeautifulSoup(raw_description, "html.parser").get_text(separator='\n')
    lines = [line.strip() for line in clean_text.splitlines() if line.strip()]
    description = "\n".join(lines) or 'No description provided.'
    if len(description) > MAX_DESCRIPTION:
        description = description[:MAX_DESCRIPTION - 3] + "..."

    location = normalize(item.get('location'))[:MAX_LOCATION] or 'TBA'

    start_time = parse_gcal_time(item['start'], default_tz)
    end_time = parse_gcal_time(item['end'], default_tz)
    if end_time <= start_time:
        end_time = start_time + datetime.timedelta(hours=1)

    return {
        'name': name,
        'description': description,
        'location': location,
        'start_time': start_time,
        'end_time': end_time,
    }


class CalendarSync(commands.Cog):
    """Mirrors the Google Calendar into Discord scheduled events.

    Each Google event ID is mapped to the Discord event it created (stored in
    STATE_FILE), so renames, time changes and cancellations update or remove
    the right Discord event instead of creating duplicates.
    """

    def __init__(self, bot):
        self.bot = bot
        self.calendar_id = os.getenv('GOOGLE_CALENDAR_ID')
        self.guild_id = int(os.getenv('GUILD_ID'))
        self.events_channel_id = int(os.getenv('EVENTS_CHANNEL_ID'))
        self._lock = asyncio.Lock()

        os.makedirs(os.path.dirname(STATE_FILE), exist_ok=True)
        self.state = self.load_state()
        self.sync_events.start()

    def cog_unload(self):
        self.sync_events.cancel()

    # ---------- state ----------

    def load_state(self):
        state = {}
        if os.path.exists(STATE_FILE):
            try:
                with open(STATE_FILE, "r") as f:
                    state = json.load(f)
            except (json.JSONDecodeError, OSError) as e:
                # Existing events get re-adopted by name, so starting fresh is safe
                print(f"State file unreadable ({e}); moving it aside and starting fresh.")
                os.replace(STATE_FILE, STATE_FILE + ".corrupt")
                state = {}
        state.setdefault("summary_message_id", None)
        state.setdefault("summary_week", None)
        state.setdefault("event_map", {})
        return state

    def save_state(self):
        tmp = STATE_FILE + ".tmp"
        with open(tmp, "w") as f:
            json.dump(self.state, f, indent=2)
        os.replace(tmp, STATE_FILE)

    # ---------- loop ----------

    @tasks.loop(minutes=15)
    async def sync_events(self):
        # A raised exception would stop tasks.loop for good, so catch everything
        async with self._lock:
            try:
                await self.run_sync()
            except Exception:
                print("Calendar sync failed:")
                traceback.print_exc()

    @sync_events.before_loop
    async def before_sync(self):
        await self.bot.wait_until_ready()

    async def run_sync(self):
        guild = self.bot.get_guild(self.guild_id)
        channel = guild.get_channel(self.events_channel_id) if guild else None
        if not guild or not channel:
            print("Error: Could not find guild or events channel.")
            return

        print("Syncing Google Calendar events to Discord...")

        try:
            gcal_events, calendar_tz = await asyncio.to_thread(
                fetch_upcoming_events, self.calendar_id, LOOKAHEAD_DAYS
            )
        except Exception as e:
            print(f"Failed to fetch calendar events: {e}")
            return

        try:
            discord_events = {e.id: e for e in await guild.fetch_scheduled_events()}
        except discord.HTTPException as e:
            print(f"Failed to fetch Discord events: {e}")
            return

        payloads = []
        for item in gcal_events:
            if item.get('status') == 'cancelled':
                continue
            try:
                payloads.append((item['id'], build_payload(item, calendar_tz)))
            except Exception as e:
                print(f"Skipping malformed calendar event {item.get('id')}: {e}")

        await self.sync_discord_events(guild, payloads, discord_events, calendar_tz)
        await self.update_summary(channel, payloads, calendar_tz)

    # ---------- Discord scheduled events ----------

    async def sync_discord_events(self, guild, payloads, discord_events, calendar_tz):
        event_map = self.state["event_map"]
        bot_id = self.bot.user.id
        now = discord.utils.utcnow()

        # Forget mappings whose Discord event no longer exists (deleted, ended, cancelled)
        for gid, did in list(event_map.items()):
            if did not in discord_events:
                del event_map[gid]
        claimed = set(event_map.values())

        seen = set()
        for gid, payload in payloads:
            seen.add(gid)
            event = discord_events.get(event_map.get(gid))

            if event is None:
                event = self.find_adoptable(payload, discord_events, claimed, bot_id)
                if event:
                    event_map[gid] = event.id
                    claimed.add(event.id)
                    self.save_state()
                    print(f"Linked existing Discord event: {event.name}")

            if event is None:
                if payload['start_time'] <= now:
                    # Discord rejects scheduled events that start in the past
                    continue
                try:
                    event = await guild.create_scheduled_event(
                        **payload,
                        entity_type=discord.EntityType.external,
                        privacy_level=discord.PrivacyLevel.guild_only,
                    )
                except discord.HTTPException as e:
                    print(f"Failed to create event '{payload['name']}': {e}")
                    continue
                discord_events[event.id] = event
                event_map[gid] = event.id
                claimed.add(event.id)
                # Save right away so a crash can never cause this one to be created twice
                self.save_state()
                print(f"Created new Discord event: {payload['name']}")
                continue

            await self.update_event(event, payload, discord_events, now)

        # Mapped events that fell out of the fetch window: cancelled, deleted, or moved
        for gid in [g for g in event_map if g not in seen]:
            event = discord_events.get(event_map[gid])
            try:
                item = await asyncio.to_thread(get_event, self.calendar_id, gid)
            except Exception as e:
                print(f"Could not look up calendar event {gid}, leaving it alone: {e}")
                continue

            if item is None or item.get('status') == 'cancelled':
                if event and event.status == discord.EventStatus.scheduled:
                    await self.delete_event(event, discord_events, "removed from Google Calendar")
                del event_map[gid]
                claimed.discard(event.id if event else None)
                continue

            payload = build_payload(item, calendar_tz)
            if payload['end_time'] <= now:
                del event_map[gid]
            elif event:
                # Moved past the lookahead window; keep the Discord event in step
                await self.update_event(event, payload, discord_events, now)

        self.save_state()

        # Bot-created events not linked to any calendar event are duplicates or
        # leftovers from renamed events. Skip if the calendar came back empty,
        # in case that was a fluke rather than a real empty calendar.
        if payloads:
            for event in list(discord_events.values()):
                if (event.id not in claimed and event.creator_id == bot_id
                        and event.status == discord.EventStatus.scheduled):
                    await self.delete_event(event, discord_events, "duplicate or no longer on Google Calendar")

    def find_adoptable(self, payload, discord_events, claimed, bot_id):
        """Finds an unlinked Discord event with the same name, closest start time first.

        Covers events created before ID tracking existed, or after the state file was lost.
        """
        key = match_key(payload['name'])
        candidates = [
            e for e in discord_events.values()
            if e.id not in claimed and match_key(e.name) == key
            and e.status in (discord.EventStatus.scheduled, discord.EventStatus.active)
        ]
        if not candidates:
            return None
        # Prefer the bot's own events, then the one closest in time
        return min(candidates, key=lambda e: (
            e.creator_id != bot_id,
            abs((e.start_time - payload['start_time']).total_seconds()),
        ))

    async def update_event(self, event, payload, discord_events, now):
        changes = {}
        if event.name != payload['name']:
            changes['name'] = payload['name']
        if (event.description or '').strip() != payload['description']:
            changes['description'] = payload['description']
        if normalize(event.location) != payload['location']:
            changes['location'] = payload['location']

        started = event.status == discord.EventStatus.active or event.start_time <= now
        if not started and event.start_time != payload['start_time']:
            changes['start_time'] = payload['start_time']
        if event.end_time != payload['end_time'] and payload['end_time'] > now:
            changes['end_time'] = payload['end_time']

        if not changes:
            return
        try:
            discord_events[event.id] = await event.edit(**changes)
            print(f"Updated Discord event '{payload['name']}': {', '.join(changes)}")
        except discord.HTTPException as e:
            print(f"Failed to update event '{payload['name']}': {e}")

    async def delete_event(self, event, discord_events, reason):
        try:
            await event.delete(reason=reason)
            discord_events.pop(event.id, None)
            print(f"Deleted Discord event '{event.name}' ({reason})")
        except discord.NotFound:
            discord_events.pop(event.id, None)
        except discord.HTTPException as e:
            print(f"Failed to delete event '{event.name}': {e}")

    # ---------- weekly summary ----------

    def current_summary_week(self, calendar_tz):
        """Date of the most recent scheduled post time, identifying this week's summary."""
        local_now = discord.utils.utcnow().astimezone(ZoneInfo(calendar_tz))
        days_back = (local_now.weekday() - SUMMARY_POST_WEEKDAY) % 7
        post_time = (local_now - datetime.timedelta(days=days_back)).replace(
            hour=SUMMARY_POST_HOUR, minute=0, second=0, microsecond=0
        )
        if post_time > local_now:
            post_time -= datetime.timedelta(days=7)
        return post_time.date().isoformat()

    async def update_summary(self, channel, payloads, calendar_tz):
        embed = create_base_embed(
            title=f"📅 Upcoming {ORG_NAME} Events (Next {SUMMARY_DAYS} Days)",
            description="Here is what we have planned for the upcoming week!",
            color=IEEE_GOLD
        )
        embed.set_footer(text=f"Last synced | {ORG_NAME} at UCF")

        now = discord.utils.utcnow()
        cutoff = now + datetime.timedelta(days=SUMMARY_DAYS)
        weekly = [p for _, p in payloads if now <= p['start_time'] <= cutoff]

        if not weekly:
            embed.add_field(name="No events scheduled.", value="Check back later!", inline=False)
        else:
            for p in weekly[:25]:
                time_str = format_timestamp(p['start_time'], style="F")
                relative_time_str = format_timestamp(p['start_time'], style="R")
                embed.add_field(
                    name=p['name'],
                    value=f"**Time:** {time_str} ({relative_time_str})\n**Location:** {p['location']}",
                    inline=False
                )

        week = self.current_summary_week(calendar_tz)
        new_week = self.state.get("summary_week") != week

        summary_message = None
        saved_msg_id = self.state.get("summary_message_id")
        if saved_msg_id:
            try:
                summary_message = await channel.fetch_message(saved_msg_id)
            except discord.NotFound:
                pass
            except discord.HTTPException as e:
                # Transient error: don't post a second summary, try again next sync
                print(f"Could not fetch summary message, will retry next sync: {e}")
                return

        if summary_message and not new_week:
            try:
                await summary_message.edit(embed=embed)
                print("Updated the weekly summary message.")
            except discord.HTTPException as e:
                print(f"Failed to edit summary message: {e}")
            return

        try:
            new_message = await channel.send(embed=embed)
        except discord.HTTPException as e:
            print(f"Failed to send summary message: {e}")
            return
        self.state["summary_message_id"] = new_message.id
        self.state["summary_week"] = week
        self.save_state()
        print(f"Posted a new weekly summary message for week of {week}.")

        # Delete last week's only after the new one is up, so there's never a gap
        if summary_message:
            try:
                await summary_message.delete()
            except discord.HTTPException as e:
                print(f"Failed to delete old summary message: {e}")


async def setup(bot):
    await bot.add_cog(CalendarSync(bot))
