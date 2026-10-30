import os
import io
import csv
import asyncio
import datetime
import difflib
from dataclasses import dataclass, field
from zoneinfo import ZoneInfo

import aiohttp
from dateutil import parser

from bot.utils.gcal_client import get_calendar_service
from bot.utils.vtools_client import fetch_branch_events, fetch_event as fetch_vtools_event
from bot.utils import knightconnect_client as kc
from bot.utils import checkin_client
from bot.cogs.calendar_sync import calendar_flyer
from bot.officer.data import EventStore, SKIPPED

# Everything the officer views need, gathered once and cached, so the board,
# event cards, digest and nudges all see the same facts.

TZ = ZoneInfo(os.getenv('REMINDER_TZ', 'America/New_York'))
BRANCH_SPOID = os.getenv('VTOOLS_SPOID', 'SBC13081')
KNIGHTCONNECT_ORG_ID = os.getenv('KNIGHTCONNECT_ORG_ID', '430881')
GITHUB_ORG = os.getenv('GITHUB_ORG', 'IEEECS-UCF')
TABLING_SHEET_ID = os.getenv('TABLING_SHEET_ID')
NOT_SLIDE_REPOS = {'website', 'ieeecs-ucf.github.io', 'ieee-cs-bot'}

WINDOW_BACK_DAYS = 45      # past events stay on the board until wrapped up or this old
WINDOW_AHEAD_DAYS = 60
SLIDES_WINDOW_DAYS = 10    # a repo created this soon after an event may hold its slides
CACHE_SECONDS = 600
VTOOLS_CONCURRENCY = 4


@dataclass
class Snapshot:
    events: list                              # calendar events in the window (dicts)
    knightconnect: dict = field(default_factory=dict)   # calendar id -> listing | None | SKIPPED
    knightconnect_orphans: list = field(default_factory=list)  # our listings with no calendar event
    vtools: dict = field(default_factory=dict)          # calendar id -> record | None | SKIPPED
    slides: dict = field(default_factory=dict)          # calendar id -> {"name", "url", "linked"?}
    checkins: dict = field(default_factory=dict)        # calendar id -> results | {"error": ...}
    discord_ids: dict = field(default_factory=dict)     # calendar id -> Discord event id
    reminders_sent: set = field(default_factory=set)    # Discord event ids already reminded
    store: dict = field(default_factory=dict)
    fetched_at: datetime.datetime = None
    problems: list = field(default_factory=list)        # sources that couldn't be read this time

    def event(self, gid):
        return next((e for e in self.events if e['id'] == gid), None)


# ---------- calendar ----------

def fetch_calendar(days_back=WINDOW_BACK_DAYS, days_ahead=WINDOW_AHEAD_DAYS, between=None):
    """Timed calendar events in a window, as dicts (tabling sessions excluded).

    between=(start, end) fetches that exact span instead of days around now.
    """
    service = get_calendar_service()
    now = datetime.datetime.now(datetime.timezone.utc)
    time_min, time_max = between or (now - datetime.timedelta(days=days_back), now + datetime.timedelta(days=days_ahead))
    items, token = [], None
    while True:
        result = service.events().list(
            calendarId=os.getenv('GOOGLE_CALENDAR_ID'),
            timeMin=time_min.isoformat(), timeMax=time_max.isoformat(),
            singleEvents=True, orderBy='startTime', maxResults=250, pageToken=token,
        ).execute(num_retries=3)
        items.extend(result.get('items', []))
        token = result.get('nextPageToken')
        if not token:
            break
    events = [to_event(item) for item in items]
    return [e for e in events if e is not None]

def to_event(item):
    """A calendar API item as the event dict the officer tools use, or None if it isn't tracked."""
    if item.get('status') == 'cancelled' or 'dateTime' not in item.get('start', {}):
        return None
    name = " ".join((item.get('summary') or 'Untitled').split())
    if 'tabling' in name.casefold():
        return None
    return {
        'id': item['id'],
        'start': parser.isoparse(item['start']['dateTime']).astimezone(TZ),
        'end': parser.isoparse(item['end']['dateTime']).astimezone(TZ),
        'name': name,
        'location': " ".join((item.get('location') or '').split()),
        'description': (item.get('description') or '').strip(),
        'flyer': calendar_flyer(item),
    }


# ---------- matching listings to events ----------

def plain(name):
    return " ".join(name.replace('-', ' ').replace('_', ' ').casefold().split())

MIN_NAME_SIMILARITY = 0.4  # a listing has to look like the event by name, not just be at the same time

def name_similarity(a, b):
    a, b = plain(a), plain(b)
    return 1.0 if a and b and (a in b or b in a) else difflib.SequenceMatcher(None, a, b).ratio()

def match_score(event, listing, start_key, title_key):
    """How well a listing fits an event (same day, similar name, near in time), or None."""
    start = parser.isoparse(listing[start_key]).astimezone(TZ)
    if start.date() != event['start'].date():
        return None
    similarity = name_similarity(event['name'], listing[title_key])
    if similarity < MIN_NAME_SIMILARITY:
        return None
    return similarity + (0.5 if abs((start - event['start']).total_seconds()) <= 2 * 3600 else 0)

def assign_listings(events, candidates_for, start_key, title_key, taken=()):
    """{event id: listing}, best fits first, each listing used by at most one event.

    taken holds listing IDs already linked to an event by hand.
    """
    pairs = []
    for e in events:
        for listing in candidates_for(e):
            score = match_score(e, listing, start_key, title_key)
            if score is not None:
                pairs.append((score, e['id'], listing))
    pairs.sort(key=lambda p: -p[0])
    found, used = {}, {str(t) for t in taken}
    for _, gid, listing in pairs:
        if gid in found or str(listing.get('id')) in used:
            continue
        found[gid] = listing
        used.add(str(listing.get('id')))
    return found

def vtools_location(record):
    return " ".join(str(record.get(k) or '') for k in ('building', 'room-number', 'address1')).strip()

def match_slides(events, repos, store=None):
    """Repo holding each event's slides: an officer's link, else by name, else a lone new repo."""
    found, claimed = {}, set()
    for e in events:
        link = (store or {}).get(e['id'], {}).get('slides')
        if link:
            found[e['id']] = dict(link, linked=True)
            claimed.add(link['name'])
    for e in events:
        if e['id'] in found:
            continue
        best, score = None, 0.0
        for r in repos:
            if r['name'] in claimed:
                continue
            ratio = difflib.SequenceMatcher(None, plain(e['name']), plain(r['name'])).ratio()
            if ratio > score:
                best, score = r, ratio
        if best and score >= 0.6:
            found[e['id']] = {'name': best['name'], 'url': best.get('html_url', '')}
            claimed.add(best['name'])
    for e in events:
        if e['id'] in found:
            continue
        window_start = e['start'] - datetime.timedelta(days=1)
        window_end = e['start'] + datetime.timedelta(days=SLIDES_WINDOW_DAYS)
        nearby = [r for r in repos if r['name'] not in claimed
                  and window_start <= parser.isoparse(r['created_at']).astimezone(TZ) <= window_end]
        if len(nearby) == 1:
            found[e['id']] = {'name': nearby[0]['name'], 'url': nearby[0].get('html_url', '')}
            claimed.add(nearby[0]['name'])
    return found


# ---------- fetching ----------

async def fetch_repos(session):
    url = f"https://api.github.com/orgs/{GITHUB_ORG}/repos?per_page=100"
    async with session.get(url, headers={"Accept": "application/vnd.github+json"},
                           timeout=aiohttp.ClientTimeout(total=30)) as resp:
        resp.raise_for_status()
        return [r for r in await resp.json() if r['name'] not in NOT_SLIDE_REPOS]

async def fetch_tabling(session):
    """Rows of the Tabling Signup sheet (shared by link, read as CSV)."""
    url = f"https://docs.google.com/spreadsheets/d/{TABLING_SHEET_ID}/export?format=csv"
    async with session.get(url, timeout=aiohttp.ClientTimeout(total=60)) as resp:
        resp.raise_for_status()
        text = await resp.text()
    rows = list(csv.reader(io.StringIO(text)))
    header_at = next(i for i, r in enumerate(rows) if 'Event Name' in r)
    header = [h.strip() for h in rows[header_at]]
    return [dict(zip(header, r)) for r in rows[header_at + 1:] if any(c.strip() for c in r)]

async def _knightconnect(session, events, store, snap):
    listings = await kc.fetch_org_events(session, KNIGHTCONNECT_ORG_ID)
    unlinked, taken = [], []
    for e in events:
        linked = store.get(e['id'], {}).get('links', {}).get('knightconnect')
        if linked == SKIPPED:
            snap.knightconnect[e['id']] = SKIPPED
        elif isinstance(linked, dict):
            record = await kc.fetch_event(session, linked['id'])
            snap.knightconnect[e['id']] = dict(record, linked=True) if record else None
            taken.append(linked['id'])
        else:
            unlinked.append(e)
    found = assign_listings(unlinked, lambda e: listings, 'startsOn', 'name', taken)
    for e in unlinked:
        snap.knightconnect[e['id']] = found.get(e['id'])
    used = {m['id'] for m in snap.knightconnect.values() if isinstance(m, dict)}
    now = datetime.datetime.now(TZ)
    snap.knightconnect_orphans = [
        l for l in listings if l['id'] not in used
        and now - datetime.timedelta(days=WINDOW_BACK_DAYS) <= parser.isoparse(l['startsOn']).astimezone(TZ)
        <= now + datetime.timedelta(days=WINDOW_AHEAD_DAYS)]

async def _vtools(session, events, store, snap):
    days = sorted({e['start'].date() for e in events})
    gate = asyncio.Semaphore(VTOOLS_CONCURRENCY)
    async def day_listings(day):
        async with gate:
            return day, await fetch_branch_events(session, day, BRANCH_SPOID)
    by_day = dict(await asyncio.gather(*(day_listings(d) for d in days)))
    unlinked, taken = [], []
    for e in events:
        linked = store.get(e['id'], {}).get('links', {}).get('vtools')
        if linked == SKIPPED:
            snap.vtools[e['id']] = SKIPPED
        elif isinstance(linked, dict):
            record = await fetch_vtools_event(session, linked['id'])
            snap.vtools[e['id']] = dict(record, linked=True) if record else None
            taken.append(linked['id'])
        else:
            unlinked.append(e)
    found = assign_listings(unlinked, lambda e: by_day.get(e['start'].date(), []), 'start-time', 'title', taken)
    for e in unlinked:
        snap.vtools[e['id']] = found.get(e['id'])

async def _slides(session, events, store, snap):
    snap.slides = match_slides(events, await fetch_repos(session), store)

async def _checkins(events, store, snap):
    for e in events:
        form = store.get(e['id'], {}).get('checkin')
        if form and checkin_client.configured():
            try:
                snap.checkins[e['id']] = await checkin_client.results(form['formId'])
            except Exception as err:
                snap.checkins[e['id']] = {"error": str(err)}

async def gather(discord_ids=None, reminders_sent=None):
    """Build a fresh Snapshot. A source that fails is noted, not fatal."""
    store = EventStore().all()
    events = await asyncio.to_thread(fetch_calendar)
    snap = Snapshot(events=events, store=store, discord_ids=discord_ids or {},
                    reminders_sent=reminders_sent or set(), fetched_at=datetime.datetime.now(TZ))
    async with aiohttp.ClientSession() as session:
        jobs = {"KnightConnect": _knightconnect(session, events, store, snap),
                "vTools": _vtools(session, events, store, snap),
                "GitHub": _slides(session, events, store, snap),
                "check-in forms": _checkins(events, store, snap)}
        results = await asyncio.gather(*jobs.values(), return_exceptions=True)
    for name, result in zip(jobs, results):
        if isinstance(result, Exception):
            snap.problems.append(f"{name}: {result}")
            print(f"Officer data: couldn't read {name}: {result}")
    return snap


class SnapshotCache:
    """One shared snapshot, refreshed when older than CACHE_SECONDS or on request."""

    def __init__(self, bot):
        self.bot = bot
        self.snapshot = None
        self._lock = asyncio.Lock()

    def _discord_links(self):
        sync = self.bot.get_cog("CalendarSync")
        reminders = self.bot.get_cog("EventReminders")
        ids = dict(sync.state["event_map"]) if sync else {}
        sent = {int(k) for k in reminders.state["sent"]} if reminders else set()
        return ids, sent

    async def get(self, fresh=False):
        async with self._lock:
            stale = (self.snapshot is None or fresh or
                     (datetime.datetime.now(TZ) - self.snapshot.fetched_at).total_seconds() > CACHE_SECONDS)
            if stale:
                ids, sent = self._discord_links()
                self.snapshot = await gather(ids, sent)
            return self.snapshot

    def with_store(self):
        """The cached snapshot with the latest officer edits, without refetching sources."""
        if self.snapshot is not None:
            self.snapshot.store = EventStore().all()
        return self.snapshot
