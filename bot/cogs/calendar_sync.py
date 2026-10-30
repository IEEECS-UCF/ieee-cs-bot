import os
import re
import json
import asyncio
import datetime
import traceback
from zoneinfo import ZoneInfo

import aiohttp
import discord
from discord.ext import commands, tasks
from dateutil import parser
from bs4 import BeautifulSoup
from bot.utils.gcal_client import (fetch_upcoming_events, get_event, insert_event, patch_event,
                                   delete_event as delete_gcal_event)
from bot.utils.formatting import create_base_embed, format_timestamp, IEEE_GOLD

ORG_NAME = "IEEE CS"
STATE_FILE = "data/state.json"
LOOKAHEAD_DAYS = 30
SUMMARY_DAYS = 7
SYNC_MINUTES = 5
# A fresh summary is posted weekly (and the old one deleted) so members get
# notified; in between, the current one is edited in place.
SUMMARY_POST_WEEKDAY = 0  # Monday
SUMMARY_POST_HOUR = 9     # in the calendar's time zone
# Never mirror more Discord deletions than this in one sync; more looks like
# an outage or a mistake, not officers cancelling events.
MAX_DELETIONS_PER_SYNC = 3
FLYER_MAX_BYTES = 8 * 1024 * 1024

# Discord limits for scheduled events
MAX_NAME = 100
MAX_LOCATION = 100
MAX_DESCRIPTION = 1000

# A line like "Flyer: https://..." in the calendar description holds the flyer
FLYER_LINE = re.compile(r'^\s*flyer:\s*(\S+)\s*$', re.IGNORECASE | re.MULTILINE)
FLYER_ANYWHERE = re.compile(r'flyer:\s*\S+', re.IGNORECASE)
FIELDS = ('name', 'description', 'location', 'start', 'end')


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

def calendar_flyer(item):
    """The flyer for a calendar event: a "Flyer:" line, or an attached image."""
    text = BeautifulSoup(item.get('description') or '', "html.parser").get_text(separator='\n')
    found = FLYER_LINE.search(text)
    if found:
        return found.group(1)
    for attachment in item.get('attachments', []):
        if (attachment.get('mimeType') or '').startswith('image/') and attachment.get('fileId'):
            return f"https://drive.google.com/uc?export=download&id={attachment['fileId']}"
    return None

def build_payload(item, default_tz):
    """Turns a Google Calendar event into the fields of a Discord scheduled event."""
    name = normalize(item.get('summary'))[:MAX_NAME] or 'Untitled Event'

    raw_description = item.get('description') or ''
    clean_text = BeautifulSoup(raw_description, "html.parser").get_text(separator='\n')
    clean_text = FLYER_LINE.sub('', clean_text)
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

def comparable(name, description, location, start, end):
    """The fields both sides share, in one form so they can be compared."""
    return {
        'name': normalize(name),
        'description': (description or '').strip() or 'No description provided.',
        'location': normalize(location) or 'TBA',
        'start': start.astimezone(datetime.timezone.utc).isoformat(),
        'end': end.astimezone(datetime.timezone.utc).isoformat(),
    }

def from_payload(payload):
    return comparable(payload['name'], payload['description'], payload['location'],
                      payload['start_time'], payload['end_time'])

def from_discord(event):
    location = event.location or (event.channel.name if event.channel else '')
    end = event.end_time or event.start_time + datetime.timedelta(hours=1)
    return comparable(event.name, event.description, location, event.start_time, end)

def cover_key(event):
    return event.cover_image.key if event.cover_image else None


class CalendarSync(commands.Cog):
    """Keeps Google Calendar and Discord scheduled events in step, both ways.

    Each Google event ID is mapped to its Discord event (STATE_FILE), with a
    snapshot of the fields as they last agreed. Whichever side differs from
    the snapshot changed, and that change is copied to the other side; if
    both changed the same field, the calendar wins. Writing back to the
    calendar needs the calendar shared with the bot as "Make changes to
    events"; until then, Discord edits are reverted to match the calendar.
    """

    def __init__(self, bot):
        self.bot = bot
        self.calendar_id = os.getenv('GOOGLE_CALENDAR_ID')
        self.guild_id = int(os.getenv('GUILD_ID'))
        self.events_channel_id = int(os.getenv('EVENTS_CHANNEL_ID'))
        self._lock = asyncio.Lock()
        self._soon = None  # pending sync after a Discord-side change
        self.can_write = False

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
        state.setdefault("snapshots", {})  # gcal ID -> fields as both sides last agreed
        return state

    def save_state(self):
        tmp = STATE_FILE + ".tmp"
        with open(tmp, "w") as f:
            json.dump(self.state, f, indent=2)
        os.replace(tmp, STATE_FILE)

    # ---------- loop ----------

    @tasks.loop(minutes=SYNC_MINUTES)
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

    # Discord-side changes sync within a minute instead of waiting for the loop
    @commands.Cog.listener()
    async def on_scheduled_event_create(self, event):
        self.sync_soon()

    @commands.Cog.listener()
    async def on_scheduled_event_update(self, before, after):
        self.sync_soon()

    @commands.Cog.listener()
    async def on_scheduled_event_delete(self, event):
        self.sync_soon()

    def sync_soon(self, delay=30):
        if self._soon and not self._soon.done():
            return
        async def later():
            await asyncio.sleep(delay)
            async with self._lock:
                try:
                    await self.run_sync()
                except Exception:
                    print("Calendar sync failed:")
                    traceback.print_exc()
        self._soon = asyncio.create_task(later())

    async def run_sync(self):
        guild = self.bot.get_guild(self.guild_id)
        channel = guild.get_channel(self.events_channel_id) if guild else None
        if not guild or not channel:
            print("Error: Could not find guild or events channel.")
            return

        try:
            gcal_events, calendar_tz, access_role = await asyncio.to_thread(
                fetch_upcoming_events, self.calendar_id, LOOKAHEAD_DAYS
            )
        except Exception as e:
            print(f"Failed to fetch calendar events: {e}")
            return
        can_write = access_role in ('writer', 'owner')
        if can_write != self.can_write:
            print("Calendar access: " + ("read/write; Discord changes sync back to the calendar."
                                         if can_write else "read-only; Discord edits are reverted to match the calendar."))
            self.can_write = can_write

        try:
            discord_events = {e.id: e for e in await guild.fetch_scheduled_events()}
        except discord.HTTPException as e:
            print(f"Failed to fetch Discord events: {e}")
            return

        items, payloads = {}, []
        for item in gcal_events:
            if item.get('status') == 'cancelled':
                continue
            try:
                payloads.append((item['id'], build_payload(item, calendar_tz)))
                items[item['id']] = item
            except Exception as e:
                print(f"Skipping malformed calendar event {item.get('id')}: {e}")

        await self.sync_discord_events(guild, items, payloads, discord_events, calendar_tz)
        if self.can_write:
            await self.import_discord_events(guild, discord_events, calendar_tz)
        await self.update_summary(channel, payloads, calendar_tz)

    # ---------- calendar <-> Discord ----------

    async def sync_discord_events(self, guild, items, payloads, discord_events, calendar_tz):
        event_map = self.state["event_map"]
        snapshots = self.state["snapshots"]
        bot_id = self.bot.user.id
        now = discord.utils.utcnow()

        # Linked Discord events that are gone. Ones whose time had passed just
        # ended; ones still upcoming were deleted by someone in Discord.
        vanished = [gid for gid, did in event_map.items() if did not in discord_events]
        upcoming = [gid for gid in vanished if gid in snapshots
                    and parser.isoparse(snapshots[gid]['end']) > now]
        # Only mirror deletions Discord confirms one by one, and never a burst of them
        confirmed = []
        if self.can_write:
            for gid in upcoming:
                if await self.confirm_deleted(guild, event_map[gid]):
                    confirmed.append(gid)
        deleted_in_discord = set(confirmed) if len(confirmed) <= MAX_DELETIONS_PER_SYNC else set()
        if upcoming and len(deleted_in_discord) < len(upcoming):
            reason = ('read-only calendar' if not self.can_write else
                      'too many at once; check Discord' if len(confirmed) > MAX_DELETIONS_PER_SYNC else
                      'Discord did not confirm the deletion')
            print(f"Not mirroring {len(upcoming) - len(deleted_in_discord)} Discord deletion(s) to the calendar ({reason}).")
        for gid in vanished:
            del event_map[gid]
        claimed = set(event_map.values())

        seen = set()
        for gid, payload in payloads:
            seen.add(gid)
            if gid in deleted_in_discord:
                try:
                    await asyncio.to_thread(delete_gcal_event, self.calendar_id, gid)
                    snapshots.pop(gid, None)
                    self.save_state()
                    print(f"Deleted calendar event '{payload['name']}' (deleted in Discord)")
                except Exception as e:
                    print(f"Failed to delete calendar event '{payload['name']}': {e}")
                continue

            event = discord_events.get(event_map.get(gid))
            if event is None:
                event = self.find_adoptable(payload, discord_events, claimed, bot_id)
                if event:
                    event_map[gid] = event.id
                    claimed.add(event.id)
                    snapshots.pop(gid, None)  # first contact: calendar wins
                    self.save_state()
                    print(f"Linked existing Discord event: {event.name}")

            if event is None:
                if payload['start_time'] <= now:
                    # Discord rejects scheduled events that start in the past
                    continue
                image = await self.download_flyer(calendar_flyer(items[gid]))
                try:
                    event = await guild.create_scheduled_event(
                        **payload,
                        entity_type=discord.EntityType.external,
                        privacy_level=discord.PrivacyLevel.guild_only,
                        **({'image': image} if image else {}),
                    )
                except discord.HTTPException as e:
                    print(f"Failed to create event '{payload['name']}': {e}")
                    continue
                discord_events[event.id] = event
                event_map[gid] = event.id
                claimed.add(event.id)
                snapshots[gid] = dict(from_payload(payload), flyer=calendar_flyer(items[gid]), cover=cover_key(event))
                # Save right away so a crash can never cause this one to be created twice
                self.save_state()
                print(f"Created new Discord event: {payload['name']}")
                continue

            await self.reconcile(gid, items[gid], payload, event, discord_events, calendar_tz, now)

        # Deleted in Discord but past the fetch window: delete those from the calendar too
        for gid in [g for g in deleted_in_discord if g not in seen]:
            try:
                await asyncio.to_thread(delete_gcal_event, self.calendar_id, gid)
                print(f"Deleted calendar event {gid} (deleted in Discord)")
            except Exception as e:
                print(f"Failed to delete calendar event {gid}: {e}")
                continue
            snapshots.pop(gid, None)

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
                snapshots.pop(gid, None)
                claimed.discard(event.id if event else None)
                continue

            payload = build_payload(item, calendar_tz)
            if payload['end_time'] <= now:
                del event_map[gid]
                snapshots.pop(gid, None)
            elif event:
                # Moved past the lookahead window; keep the two in step
                await self.reconcile(gid, item, payload, event, discord_events, calendar_tz, now)

        # Forget snapshots of events that are over and no longer linked
        for gid in [g for g in snapshots if g not in event_map]:
            if parser.isoparse(snapshots[gid]['end']) <= now:
                del snapshots[gid]
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

    async def reconcile(self, gid, item, payload, event, discord_events, calendar_tz, now):
        """Copy whichever side changed since the last sync to the other side."""
        snapshots = self.state["snapshots"]
        snap = snapshots.get(gid)
        cal = from_payload(payload)
        dis = from_discord(event)
        cal_flyer = calendar_flyer(item)
        started = event.status == discord.EventStatus.active or event.start_time <= now

        to_discord, to_calendar = {}, {}
        for field in FIELDS:
            if cal[field] == dis[field]:
                continue
            calendar_changed = snap is None or cal[field] != snap[field]
            if calendar_changed or not self.can_write:
                to_discord[field] = cal[field]   # calendar wins ties, and always when read-only
            elif dis[field] != snap[field]:
                to_calendar[field] = dis[field]
        if started:
            # Discord can't move an event that has started, and its times are final
            to_discord.pop('start', None)
            to_calendar.pop('start', None)
            to_calendar.pop('end', None)

        new_cover = None
        if cal_flyer and (snap is None or cal_flyer != snap.get('flyer')):
            new_cover = await self.download_flyer(cal_flyer)
        elif snap is not None and cover_key(event) != snap.get('cover') and cover_key(event) and self.can_write:
            to_calendar['flyer'] = event.cover_image.url

        if to_discord or new_cover:
            changes = {}
            if 'name' in to_discord:
                changes['name'] = payload['name']
            if 'description' in to_discord:
                changes['description'] = payload['description']
            if 'location' in to_discord:
                changes['location'] = payload['location']
            if 'start' in to_discord:
                changes['start_time'] = payload['start_time']
            if 'end' in to_discord and payload['end_time'] > now:
                changes['end_time'] = payload['end_time']
            if new_cover:
                changes['image'] = new_cover
            if changes:
                try:
                    event = await event.edit(**changes)
                    discord_events[event.id] = event
                    print(f"Updated Discord event '{payload['name']}': {', '.join(changes)}")
                except discord.HTTPException as e:
                    print(f"Failed to update Discord event '{payload['name']}': {e}")
                    return

        if to_calendar:
            body = {}
            if 'name' in to_calendar:
                body['summary'] = event.name
            if 'location' in to_calendar:
                body['location'] = to_calendar['location']
            if 'description' in to_calendar or 'flyer' in to_calendar:
                # A new description comes from Discord; a new flyer alone only swaps the Flyer line
                if 'description' in to_calendar:
                    text = (event.description or '').strip()
                else:
                    text = FLYER_ANYWHERE.sub('', item.get('description') or '').strip()
                flyer = to_calendar.get('flyer', cal_flyer)
                body['description'] = text + (f"\n\nFlyer: {flyer}" if flyer else '')
            if 'start' in to_calendar:
                body['start'] = {'dateTime': event.start_time.isoformat(), 'timeZone': calendar_tz}
            if 'end' in to_calendar and event.end_time:
                body['end'] = {'dateTime': event.end_time.isoformat(), 'timeZone': calendar_tz}
            try:
                item = await asyncio.to_thread(patch_event, self.calendar_id, gid, body)
                payload = build_payload(item, calendar_tz)
                cal_flyer = calendar_flyer(item)
                print(f"Updated calendar event '{event.name}' from Discord: {', '.join(to_calendar)}")
            except Exception as e:
                print(f"Failed to update calendar event '{event.name}': {e}")
                return

        snapshots[gid] = dict(from_payload(payload), flyer=cal_flyer, cover=cover_key(event))
        self.save_state()

    async def import_discord_events(self, guild, discord_events, calendar_tz):
        """Events officers created in Discord get a calendar event too."""
        event_map = self.state["event_map"]
        linked = set(event_map.values())
        bot_id = self.bot.user.id
        for event in discord_events.values():
            if (event.id in linked or event.creator_id == bot_id
                    or event.status != discord.EventStatus.scheduled):
                continue
            fields = from_discord(event)
            body = {
                'summary': event.name,
                'location': fields['location'],
                'description': (event.description or '').strip()
                               + (f"\n\nFlyer: {event.cover_image.url}" if event.cover_image else ''),
                'start': {'dateTime': event.start_time.isoformat(), 'timeZone': calendar_tz},
                'end': {'dateTime': parser.isoparse(fields['end']).isoformat(), 'timeZone': calendar_tz},
            }
            try:
                item = await asyncio.to_thread(insert_event, self.calendar_id, body)
            except Exception as e:
                print(f"Failed to add Discord event '{event.name}' to the calendar: {e}")
                continue
            event_map[item['id']] = event.id
            self.state["snapshots"][item['id']] = dict(from_payload(build_payload(item, calendar_tz)),
                                                       flyer=calendar_flyer(item), cover=cover_key(event))
            self.save_state()
            print(f"Added Discord event '{event.name}' to the calendar")

    async def download_flyer(self, url):
        """Image bytes for a flyer link, or None if it can't be fetched as an image."""
        if not url:
            return None
        try:
            async with aiohttp.ClientSession() as session:
                async with session.get(url, timeout=aiohttp.ClientTimeout(total=30)) as resp:
                    kind = resp.headers.get('Content-Type', '')
                    if resp.status != 200 or not kind.startswith('image/'):
                        print(f"Flyer link isn't a downloadable image ({resp.status}, {kind or 'no type'}): {url}")
                        return None
                    data = await resp.content.read(FLYER_MAX_BYTES + 1)
        except Exception as e:
            print(f"Couldn't download flyer {url}: {e}")
            return None
        if len(data) > FLYER_MAX_BYTES:
            print(f"Flyer too large (over {FLYER_MAX_BYTES // 1024 // 1024} MB): {url}")
            return None
        return data

    async def confirm_deleted(self, guild, discord_id):
        """True only if Discord says the event is gone (or cancelled), not on an error."""
        try:
            event = await guild.fetch_scheduled_event(discord_id)
        except discord.NotFound:
            return True
        except discord.HTTPException as e:
            print(f"Could not confirm Discord event {discord_id} was deleted: {e}")
            return False
        return event.status == discord.EventStatus.cancelled

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
