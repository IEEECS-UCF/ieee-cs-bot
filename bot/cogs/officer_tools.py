import os
import io
import re
import csv
import json
import asyncio
import datetime
import difflib
import traceback
from zoneinfo import ZoneInfo

import aiohttp
import discord
from discord import app_commands
from discord.ext import commands, tasks
from dateutil import parser

from bot.utils.gcal_client import get_calendar_service
from bot.utils.formatting import create_base_embed, IEEE_BLUE, IEEE_GOLD
from bot.utils.vtools_client import fetch_branch_events, fetch_event, parse_event_id
from bot.utils import checkin_client
from bot.utils import knightconnect_client as kc
from bot.utils import event_text
from bot.utils.event_text import place, same_place
from bot.cogs.calendar_sync import calendar_flyer
from bot.cogs import volunteer

TZ = ZoneInfo(os.getenv('REMINDER_TZ', 'America/New_York'))
BRANCH_SPOID = os.getenv('VTOOLS_SPOID', 'SBC13081')
TABLING_SHEET_ID = os.getenv('TABLING_SHEET_ID')
DEADLINES_FILE = "bot/deadlines.json"
DIGEST_STATE_FILE = "data/digest.json"
CHECKINS_FILE = "data/checkins.json"  # calendar event ID -> check-in form
WRAPUPS_FILE = "data/wrapups.json"    # calendar event ID -> items officers marked or linked
DEADLINES_EXTRA_FILE = "data/deadlines_extra.json"  # deadlines added or hidden from Discord
OWNERS_FILE = "data/owners.json"      # calendar event ID -> Discord user ID of its owner
# Default owner by kind of event ("workshops:<id>,other:<id>"); /officer owner overrides it
DEFAULT_OWNERS = dict(pair.split(":", 1) for pair in os.getenv('DEFAULT_OWNERS', '').split(",") if ":" in pair)
NUDGES_FILE = "data/nudges.json"      # {"sent": {"<event>|<item>": date}, "threads": {event: thread ID}}
GITHUB_ORG = os.getenv('GITHUB_ORG', 'IEEECS-UCF')
KNIGHTCONNECT_ORG_ID = os.getenv('KNIGHTCONNECT_ORG_ID', '430881')
# Only these roles may use /officer, whatever Discord's command permissions say
# (Officer, Exec). Discord's own setting (Manage Events) is a second layer.
OFFICER_ROLE_IDS = {int(r) for r in os.getenv('OFFICER_ROLE_IDS', '1449921163248533525,1424610929424994477').split(',') if r.strip()}
NOT_SLIDE_REPOS = {'website', 'ieeecs-ucf.github.io', 'ieee-cs-bot'}

VTOOLS_LOOKBACK_DAYS = 90
EVENTS_LOOKAHEAD_DAYS = 45
ROOM_WARNING_DAYS = 21     # flag events this close with no room
TABLING_LOOKAHEAD_DAYS = 60
DEADLINE_LOOKAHEAD_DAYS = 90
# Days the officer digest posts (DIGEST_DAYS, e.g. "sun,wed"), at DIGEST_HOUR local time
WEEKDAYS = {"mon": 0, "tue": 1, "wed": 2, "thu": 3, "fri": 4, "sat": 5, "sun": 6}
DIGEST_DAYS = {WEEKDAYS[d.strip()[:3].lower()] for d in os.getenv('DIGEST_DAYS', 'sun,wed').split(",")
               if d.strip()[:3].lower() in WEEKDAYS}
DIGEST_HOUR = 10
CHECKIN_PICK_BACK_DAYS = 2    # events offered by /officer checkin create
CHECKIN_PICK_AHEAD_DAYS = 30
WRAPUP_LOOKBACK_DAYS = 21     # events still shown in the wrap-up checklist
SLIDES_WINDOW_DAYS = 10       # a repo created this soon after an event may hold its slides
CROSSCHECK_BACK_DAYS = 14     # listings compared against the calendar
CROSSCHECK_AHEAD_DAYS = 45
LISTING_DUE_DAYS = 21         # flag a missing listing once the event is this close
NUDGE_HOUR = 11               # local time, after the reminders and the digest
NUDGE_REPEAT_DAYS = 7         # mention an unresolved item again after this long
CHECKIN_DUE_DAYS = 2          # nudge for a check-in form this close to the event

EMBED_LIMIT = 4000


# ---------- data sources ----------

def fetch_calendar(days_back, days_ahead):
    """Google Calendar events in a window, as (start, end, name, location, description)."""
    service = get_calendar_service()
    now = datetime.datetime.now(datetime.timezone.utc)
    items, token = [], None
    while True:
        result = service.events().list(
            calendarId=os.getenv('GOOGLE_CALENDAR_ID'),
            timeMin=(now - datetime.timedelta(days=days_back)).isoformat(),
            timeMax=(now + datetime.timedelta(days=days_ahead)).isoformat(),
            singleEvents=True, orderBy='startTime', maxResults=250, pageToken=token,
        ).execute(num_retries=3)
        items.extend(result.get('items', []))
        token = result.get('nextPageToken')
        if not token:
            break
    events = []
    for item in items:
        if item.get('status') == 'cancelled' or 'dateTime' not in item.get('start', {}):
            continue
        events.append({
            'id': item['id'],
            'start': parser.isoparse(item['start']['dateTime']).astimezone(TZ),
            'end': parser.isoparse(item['end']['dateTime']).astimezone(TZ),
            'name': " ".join((item.get('summary') or 'Untitled').split()),
            'location': " ".join((item.get('location') or '').split()),
            'description': (item.get('description') or '').strip(),
            'flyer': calendar_flyer(item),
        })
    return events

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

def load_json(path, default):
    if not os.path.exists(path):
        return default
    with open(path) as f:
        return json.load(f)

def save_json(path, data):
    tmp = path + ".tmp"
    with open(tmp, "w") as f:
        json.dump(data, f, indent=2)
    os.replace(tmp, path)

def load_checkins():
    return load_json(CHECKINS_FILE, {})

def save_checkins(checkins):
    save_json(CHECKINS_FILE, checkins)

def load_wrapups():
    return load_json(WRAPUPS_FILE, {})

def save_wrapups(wrapups):
    save_json(WRAPUPS_FILE, wrapups)

async def fetch_repos(session):
    """The org's public GitHub repos (unauthenticated: 60 requests/hour is plenty)."""
    url = f"https://api.github.com/orgs/{GITHUB_ORG}/repos?per_page=100"
    async with session.get(url, headers={"Accept": "application/vnd.github+json"},
                           timeout=aiohttp.ClientTimeout(total=30)) as resp:
        resp.raise_for_status()
        return [r for r in await resp.json() if r['name'] not in NOT_SLIDE_REPOS]

def deadline_key(day, what):
    return f"{day.isoformat()}|{what}"

def load_deadlines():
    """The shipped list plus deadlines officers added, minus ones they hid."""
    extra = load_json(DEADLINES_EXTRA_FILE, {"added": [], "hidden": []})
    entries = load_json(DEADLINES_FILE, []) + extra["added"]
    hidden = set(extra["hidden"])
    deadlines = [(datetime.date.fromisoformat(d['date']), d['what']) for d in entries]
    return [d for d in deadlines if deadline_key(*d) not in hidden]


# ---------- checks (each returns lines of text) ----------

def fmt_day(dt):
    return dt.strftime('%a %b %-d')

def no_room(location):
    return location.casefold() in ('', 'tba', 'tbd')

def owner_of(event, owners):
    """(user ID or None, set by hand?) for an event."""
    if event['id'] in owners:
        return owners[event['id']], True
    # Events that aren't a workshop, social or career event (GBMs, TechX) use "other"
    default = DEFAULT_OWNERS.get(event_text.kind(event) or 'other')
    return (int(default), False) if default else (None, False)

def check_events(events):
    now = datetime.datetime.now(TZ)
    owners = load_json(OWNERS_FILE, {})
    upcoming = [e for e in events if e['start'] > now]
    lines = []
    for e in upcoming:
        days = (e['start'].date() - now.date()).days
        issues = []
        if no_room(e['location']):
            issues.append("**no room**" if days <= ROOM_WARNING_DAYS else "no room yet")
        if not e['description']:
            issues.append("no description")
        for other in upcoming:
            if other is e or not (e['start'] < other['end'] and other['start'] < e['end']):
                continue
            same_room = e['location'] and e['location'].casefold() == other['location'].casefold()
            issues.append(f"**{'same room as' if same_room else 'overlaps'} {other['name']}**")
        due = []
        for what, lead in (("signage/IG", 14), ("announce", 7)):
            when = e['start'].date() - datetime.timedelta(days=lead)
            due.append(f"{what} {'was due' if when < now.date() else 'by'} {when:%b %-d}")
        status = "; ".join(issues) or "ok"
        who, by_hand = owner_of(e, owners)
        owner = (f" · owner <@{who}>" + ("" if by_hand else " (default)")) if who else " · **no owner**"
        lines.append(f"`{fmt_day(e['start'])}` **{e['name']}** ({e['location'] or 'TBA'}){owner}: {status}"
                     + (f"\n  ↳ {', '.join(due)}" if due else ""))
    return lines or ["No upcoming events in the calendar."]

async def checkin_note(event, checkins):
    """Check-in counts to copy into vTools, if the event had a check-in form."""
    form = checkins.get(event.get('id'))
    if not form or not checkin_client.configured():
        return ""
    try:
        r = await checkin_client.results(form['formId'])
    except Exception as e:
        return f" · check-in unavailable ({e})"
    return f" · check-in: **{r['ieee']} IEEE, {r['guests']} guests**"

def reportable(events):
    """Past events that should be in vTools (tabling isn't an event we report)."""
    now = datetime.datetime.now(TZ)
    return [e for e in events if e['end'] < now and 'tabling' not in e['name'].casefold()]

SKIPPED = "not needed"

async def vtools_matches(events):
    """Per calendar event ID: the vTools record, None, or SKIPPED.

    An officer's link (/officer wrapup vtools) wins over the automatic match.
    """
    marks = load_wrapups()
    matches, cache = {}, {}
    async with aiohttp.ClientSession() as session:
        for e in events:
            linked = marks.get(e['id'], {}).get('vtools')
            if linked == SKIPPED:
                matches[e['id']] = SKIPPED
                continue
            if isinstance(linked, dict):
                record = await fetch_event(session, linked['id'])
                if record:
                    matches[e['id']] = dict(record, linked=True)
                    continue
            day = e['start'].date()
            if day not in cache:
                cache[day] = await fetch_branch_events(session, day, BRANCH_SPOID)
            matches[e['id']] = best_match(e, cache[day])
    return matches

async def check_vtools(events):
    checkins = load_checkins()
    past = reportable(events)
    matches = await vtools_matches(past)
    lines = []
    for e in past:
        match = matches[e['id']]
        label = f"`{fmt_day(e['start'])}` {e['name']}"
        if match == SKIPPED:
            lines.append(f"➖ {label}: marked not needed")
        elif match is None:
            lines.append(f"❌ {label}: **not in vTools**" + await checkin_note(e, checkins))
        elif match.get('ieee-attending') is None:
            lines.append(f"⚠️ {label}: in vTools, **no attendance filed** · <{match['link']}>"
                         + await checkin_note(e, checkins))
        else:
            ieee, guests = match['ieee-attending'], match.get('guests-attending') or 0
            linked = " (linked)" if match.get('linked') else ""
            lines.append(f"✅ {label}: {ieee + guests} ({ieee} IEEE, {guests} guests){linked}")
    return lines or [f"No calendar events in the last {VTOOLS_LOOKBACK_DAYS} days."]

def best_match(event, candidates, start_key='start-time', title_key='title'):
    """The listing closest in name, or in start time if names differ."""
    best, best_score = None, 0.0
    for c in candidates:
        start = parser.isoparse(c[start_key]).astimezone(TZ)
        if start.date() != event['start'].date():
            continue
        score = difflib.SequenceMatcher(None, event['name'].casefold(), c[title_key].casefold()).ratio()
        if abs((start - event['start']).total_seconds()) <= 2 * 3600:
            score += 0.5
        if score > best_score:
            best, best_score = c, score
    return best if best_score >= 0.5 else None

async def check_tabling():
    if not TABLING_SHEET_ID:
        return ["Tabling sheet not configured (TABLING_SHEET_ID)."]
    async with aiohttp.ClientSession() as session:
        rows = await fetch_tabling(session)
    today = datetime.datetime.now(TZ).date()
    lines = []
    for r in rows:
        try:
            day = datetime.date.fromisoformat(r.get('Date', '').strip())
        except ValueError:
            continue
        if not (0 <= (day - today).days <= TABLING_LOOKAHEAD_DAYS):
            continue
        people = [r.get(k, '').strip() for k in ('Volunteer 1', 'Volunteer 2')]
        filled = [p for p in people if p]
        mark = "🔴" if not filled else ("🟡" if len(filled) < 2 else "🟢")
        who = ", ".join(filled) or "**nobody yet**"
        lines.append(f"{mark} `{day:%a %b %-d}` {r.get('Event Name', '').strip()} {r.get('Time', '').strip()}: {who}")
    return lines or ["No tabling sessions in the next two months."]

def check_deadlines():
    today = datetime.datetime.now(TZ).date()
    lines = []
    for day, what in sorted(load_deadlines()):
        left = (day - today).days
        if 0 <= left <= DEADLINE_LOOKAHEAD_DAYS:
            lines.append(f"`{day:%a %b %-d}` ({left}d) {what}")
    return lines or ["No deadlines in the next 90 days."]


DONE, TODO, SKIP, LATER = "✅", "⬜", "➖", "⏳"

# ---------- listings check: KnightConnect and vTools vs. the calendar ----------

def vtools_location(record):
    return " ".join(str(record.get(k) or '') for k in ('building', 'room-number', 'address1')).strip()

def compare_listing(event, start, location, title):
    """Differences between a calendar event and a listing, as short phrases."""
    diffs = []
    if abs((start - event['start']).total_seconds()) > 60:
        diffs.append(f"time {start:%a %b %-d %-I:%M %p} vs calendar {event['start']:%-I:%M %p}")
    if location and event['location'] and not no_room(event['location']) and not same_place(location, event['location']):
        diffs.append(f"room **{location}** vs calendar **{event['location']}**")
    ours, theirs = plain(event['name']), plain(title)
    if ours not in theirs and theirs not in ours and difflib.SequenceMatcher(None, ours, theirs).ratio() < 0.4:
        diffs.append(f"title \"{title}\"")
    return diffs

async def knightconnect_matches(events):
    """Per calendar event ID: the KnightConnect listing, None, or SKIPPED (officer links win).

    Also returns the org's listings in the same window that match no calendar event.
    """
    marks = load_wrapups()
    matches = {}
    async with aiohttp.ClientSession() as session:
        listings = await kc.fetch_org_events(session, KNIGHTCONNECT_ORG_ID)
        for e in events:
            linked = marks.get(e['id'], {}).get('knightconnect')
            if linked == SKIPPED:
                matches[e['id']] = SKIPPED
                continue
            if isinstance(linked, dict):
                record = await kc.fetch_event(session, linked['id'])
                if record:
                    matches[e['id']] = dict(record, linked=True)
                    continue
            matches[e['id']] = best_match(e, listings, start_key='startsOn', title_key='name')
    now = datetime.datetime.now(TZ)
    used = {m['id'] for m in matches.values() if isinstance(m, dict)}
    orphans = [l for l in listings if l['id'] not in used
               and now - datetime.timedelta(days=CROSSCHECK_BACK_DAYS)
               <= parser.isoparse(l['startsOn']).astimezone(TZ)
               <= now + datetime.timedelta(days=CROSSCHECK_AHEAD_DAYS)]
    return matches, orphans

def listing_status(event, match, start_key, title_key, location_of):
    now = datetime.datetime.now(TZ)
    if match == SKIPPED:
        return SKIP, "not needed"
    if match is None:
        due = (event['start'] - now).days <= LISTING_DUE_DAYS
        return (TODO if due else LATER), ("**missing**" if due else "not listed yet")
    diffs = compare_listing(event, parser.isoparse(match[start_key]).astimezone(TZ),
                            location_of(match), match[title_key])
    linked = " (linked)" if match.get('linked') else ""
    if diffs:
        return "⚠️", "; ".join(diffs) + f" · <{match['link']}>"
    return DONE, "matches" + linked

async def crosscheck_rows(events):
    """[(event, kc_state, kc_note, vt_state, vt_note)] for recent and upcoming events."""
    now = datetime.datetime.now(TZ)
    window = [e for e in events if 'tabling' not in e['name'].casefold()
              and now - datetime.timedelta(days=CROSSCHECK_BACK_DAYS) <= e['start'] <= now + datetime.timedelta(days=CROSSCHECK_AHEAD_DAYS)]
    kc_matches, kc_orphans = await knightconnect_matches(window)
    vt_matches = await vtools_matches(window)
    rows = []
    for e in window:
        kc_state, kc_note = listing_status(e, kc_matches[e['id']], 'startsOn', 'name', lambda m: m.get('location'))
        vt_state, vt_note = listing_status(e, vt_matches[e['id']], 'start-time', 'title', vtools_location)
        rows.append((e, kc_state, kc_note, vt_state, vt_note))
    return rows, kc_orphans

def format_crosscheck(result, problems_only=False):
    rows, kc_orphans = result
    lines = []
    for e, kc_state, kc_note, vt_state, vt_note in rows:
        if problems_only and kc_state in (DONE, SKIP, LATER) and vt_state in (DONE, SKIP, LATER):
            continue
        lines.append(f"`{fmt_day(e['start'])}` **{e['name']}**\n"
                     f"  KnightConnect: {kc_state} {kc_note}\n  vTools: {vt_state} {vt_note}")
    for listing in kc_orphans:
        start = parser.isoparse(listing['startsOn']).astimezone(TZ)
        lines.append(f"`{fmt_day(start)}` **{listing['name']}**\n  ⚠️ on KnightConnect ({listing.get('location') or 'no room'}) "
                     f"but **not in the calendar** · <{listing['link']}>")
    if not lines:
        return ["Every listing matches the calendar."] if rows else ["No events in the window."]
    return lines


# ---------- post-event wrap-up ----------

SOCIAL_WORDS = ('social', 'game night', 'picnic', 'party', 'mixer', 'bbq', 'tailgate')

def plain(name):
    return " ".join(name.replace('-', ' ').replace('_', ' ').casefold().split())

def match_slides(events, repos, marks=None):
    """Repo holding each event's slides.

    An officer's link (/officer wrapup slides) wins; otherwise match by name,
    then by a lone repo created soon after the event.
    """
    found, claimed = {}, set()
    for e in events:
        link = (marks or {}).get(e['id'], {}).get('slides_link')
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
            found[e['id']] = best
            claimed.add(best['name'])
    for e in events:
        if e['id'] in found:
            continue
        window_start = e['start'] - datetime.timedelta(days=1)
        window_end = e['start'] + datetime.timedelta(days=SLIDES_WINDOW_DAYS)
        nearby = [r for r in repos if r['name'] not in claimed
                  and window_start <= parser.isoparse(r['created_at']).astimezone(TZ) <= window_end]
        if len(nearby) == 1:
            found[e['id']] = nearby[0]
            claimed.add(nearby[0]['name'])
    return found

async def wrapup_rows(events):
    """[(event, [(item, state, note)])] for events that ended in the last few weeks."""
    now = datetime.datetime.now(TZ)
    recent = [e for e in reportable(events) if e['end'] >= now - datetime.timedelta(days=WRAPUP_LOOKBACK_DAYS)]
    if not recent:
        return []
    matches = await vtools_matches(recent)
    async with aiohttp.ClientSession() as session:
        try:
            repos = await fetch_repos(session)
        except Exception as e:
            print(f"GitHub repo list failed: {e}")
            repos = []
    checkins, marks = load_checkins(), load_wrapups()
    slides = match_slides(recent, repos, marks)

    rows = []
    for e in recent:
        mine = marks.get(e['id'], {})
        items = []
        form = checkins.get(e['id'])
        if form:
            try:
                r = await checkin_client.results(form['formId'])
                items.append(("check-in", DONE, f"{r['total']} ({r['ieee']} IEEE)"))
            except Exception:
                items.append(("check-in", TODO, "unreadable"))
        match = matches[e['id']]
        if match == SKIPPED:
            items.append(("vTools", SKIP, ""))
        else:
            filed = match is not None and match.get('ieee-attending') is not None
            items.append(("vTools", DONE if filed else TODO, "linked" if match and match.get('linked') else ""))
        if any(w in e['name'].casefold() for w in SOCIAL_WORDS) or mine.get('slides') == 'not needed':
            items.append(("slides", SKIP, ""))
        elif e['id'] in slides:
            items.append(("slides", DONE, slides[e['id']]['name'] + (", linked" if slides[e['id']].get('linked') else "")))
        else:
            items.append(("slides", DONE if mine.get('slides') == 'done' else TODO, ""))
        recap = mine.get('recap')
        items.append(("recap", SKIP if recap == 'not needed' else (DONE if recap == 'done' else TODO), ""))
        rows.append((e, items))
    return rows

def format_wrapups(rows, open_only=False):
    lines = []
    for e, items in rows:
        if open_only and not any(state == TODO for _, state, _ in items):
            continue
        parts = [f"{state} {item}" + (f" ({note})" if note else "") for item, state, note in items]
        lines.append(f"`{fmt_day(e['start'])}` **{e['name']}**: " + " · ".join(parts))
    if not lines:
        return ["All wrapped up."] if rows else [f"No events in the last {WRAPUP_LOOKBACK_DAYS} days."]
    return lines


# ---------- nudges ----------

async def open_items(events):
    """{event ID: [what's still open, with the command that fixes it]} for active events."""
    now = datetime.datetime.now(TZ)
    checkins = load_checkins()
    crosscheck, _ = await crosscheck_rows(events)
    listing = {e['id']: (kc_state, vt_state) for e, kc_state, _, vt_state, _ in crosscheck}
    wrapups = {e['id']: items for e, items in await wrapup_rows(events)}
    found = {}
    for e in events:
        if 'tabling' in e['name'].casefold():
            continue
        items = []
        days = (e['start'].date() - now.date()).days
        if e['start'] > now:
            if no_room(e['location']) and days <= ROOM_WARNING_DAYS:
                items.append(("room", "no room booked yet"))
            kc_state, vt_state = listing.get(e['id'], (None, None))
            if kc_state == TODO:
                items.append(("knightconnect", "not on KnightConnect: `/officer listings copy`"))
            if vt_state == TODO:
                items.append(("vtools", "not in vTools: `/officer listings copy`"))
            if e['id'] not in checkins and days <= CHECKIN_DUE_DAYS:
                items.append(("checkin", "no check-in form: `/officer checkin create`"))
        elif e['end'] <= now - datetime.timedelta(days=1):
            for item, state, _ in wrapups.get(e['id'], []):
                if state == TODO:
                    fix = {"vTools": "file the attendance report in vTools",
                           "slides": "post slides to GitHub, or `/officer wrapup slides`",
                           "recap": "post a recap, then `/officer wrapup mark`"}.get(item, item)
                    items.append((f"wrapup-{item}", fix))
        if items:
            found[e['id']] = items
    return found

def due_nudges(items_by_event, sent, today):
    """Only items not mentioned in the last NUDGE_REPEAT_DAYS."""
    due = {}
    for gid, items in items_by_event.items():
        fresh = [(key, text) for key, text in items
                 if f"{gid}|{key}" not in sent
                 or (today - datetime.date.fromisoformat(sent[f"{gid}|{key}"])).days >= NUDGE_REPEAT_DAYS]
        if fresh:
            due[gid] = fresh
    return due


def digest_due(now, last_day):
    """Post on a digest day, once, after DIGEST_HOUR."""
    return (now.weekday() in DIGEST_DAYS and now.hour >= DIGEST_HOUR
            and last_day != now.date().isoformat())


MESSAGE_EMBED_CHARS = 5800  # Discord allows 6000 across all embeds in one message
MESSAGE_EMBEDS = 10

def embed_size(embed):
    return len(embed.title or "") + len(embed.description or "") + len(embed.footer.text or "")

def split_embeds(embeds):
    """Group embeds into messages that stay under Discord's per-message limits."""
    batches, current, size = [], [], 0
    for embed in embeds:
        n = embed_size(embed)
        if current and (size + n > MESSAGE_EMBED_CHARS or len(current) == MESSAGE_EMBEDS):
            batches.append(current)
            current, size = [], 0
        current.append(embed)
        size += n
    if current:
        batches.append(current)
    return batches


def build_embed(title, lines, color=IEEE_BLUE, footer=None):
    text = ""
    for line in lines:
        if len(text) + len(line) + 1 > EMBED_LIMIT:
            text += "\n…"
            break
        text += line + "\n"
    embed = create_base_embed(title=title, description=text, color=color)
    if footer:
        embed.set_footer(text=footer)
    return embed


def mentions_in(text):
    """Allow exactly the mentions written into an approved post, nothing else."""
    roles = [discord.Object(int(r)) for r in re.findall(r'<@&(\d+)>', text)]
    return discord.AllowedMentions(everyone='@everyone' in text, users=False, roles=roles or False)

class DraftEdit(discord.ui.Modal, title="Edit the announcement"):
    def __init__(self, view):
        super().__init__(timeout=900)
        self.view = view
        self.body = discord.ui.TextInput(label="Announcement", style=discord.TextStyle.paragraph,
                                         default=view.draft[:2000], max_length=2000)
        self.add_item(self.body)

    async def on_submit(self, interaction: discord.Interaction):
        url = await self.view.post_once(interaction, self.body.value)
        await interaction.response.send_message(url or "Already posted from this draft.", ephemeral=True)

class DraftView(discord.ui.View):
    """Buttons under a draft (only the officer who asked sees them). Posts at most once."""
    def __init__(self, draft, channel_id):
        super().__init__(timeout=900)
        self.draft, self.channel_id, self.posted = draft, channel_id, False

    async def post_once(self, interaction, text):
        if self.posted:
            return None
        channel = interaction.client.get_channel(self.channel_id)
        if channel is None:
            return "Couldn't find the announcements channel."
        self.posted = True
        self.stop()
        message = await channel.send(text, allowed_mentions=mentions_in(text))
        print(f"Announcement posted by {interaction.user} ({interaction.user.id})")
        return f"Posted: {message.jump_url}"

    @discord.ui.button(label="Post as is", style=discord.ButtonStyle.primary)
    async def post(self, interaction: discord.Interaction, button: discord.ui.Button):
        result = await self.post_once(interaction, self.draft)
        await interaction.response.edit_message(content=result or "Already posted from this draft.", view=None)

    @discord.ui.button(label="Edit and post", style=discord.ButtonStyle.secondary)
    async def edit(self, interaction: discord.Interaction, button: discord.ui.Button):
        if self.posted:
            await interaction.response.send_message("Already posted from this draft.", ephemeral=True)
            return
        await interaction.response.send_modal(DraftEdit(self))


class OfficerTools(commands.Cog):
    """Officer checklists, shown privately to whoever runs the command.

    The officer digest (DIGEST_DAYS) only posts if OFFICER_DIGEST_CHANNEL_ID is set.
    """

    officer = app_commands.Group(
        name="officer", description="Officer checklists (only you can see the replies).",
        default_permissions=discord.Permissions(manage_events=True),
    )

    checkin = app_commands.Group(
        name="checkin", description="Event check-in forms and attendance counts.", parent=officer,
    )
    wrapup = app_commands.Group(
        name="wrapup", description="After-event checklist: vTools, slides, recap.", parent=officer,
    )
    reminders = app_commands.Group(
        name="reminders", description="Day-of event reminders: preview and per-event settings.", parent=officer,
    )
    deadlines = app_commands.Group(
        name="deadlines", description="Chapter deadlines shown in the digest.", parent=officer,
    )
    listings = app_commands.Group(
        name="listings", description="KnightConnect and vTools listings vs. the calendar.", parent=officer,
    )
    task = app_commands.Group(
        name="task", description="Volunteer board: tasks members can claim.", parent=officer,
    )

    def __init__(self, bot):
        self.bot = bot
        self._pick_cache = (None, [])  # (fetched at, events) for autocomplete
        self._recent_cache = (None, [])
        self._repo_cache = (None, [])
        channel = os.getenv('OFFICER_DIGEST_CHANNEL_ID')
        self.digest_channel_id = int(channel) if channel else None
        if self.digest_channel_id:
            self.officer_digest.start()
        else:
            print("Officer digest disabled: OFFICER_DIGEST_CHANNEL_ID not set.")
        nudge_channel = os.getenv('NUDGE_CHANNEL_ID')
        self.nudge_channel_id = int(nudge_channel) if nudge_channel else None
        if self.nudge_channel_id:
            self.daily_nudges.start()
        else:
            print("Event nudges disabled: NUDGE_CHANNEL_ID not set.")

    def cog_unload(self):
        self.officer_digest.cancel()
        self.daily_nudges.cancel()

    async def interaction_check(self, interaction: discord.Interaction) -> bool:
        """Every /officer command: Officer or Exec role required."""
        roles = {role.id for role in getattr(interaction.user, 'roles', [])}
        if roles & OFFICER_ROLE_IDS:
            return True
        print(f"Refused /officer for {interaction.user} ({interaction.user.id}): no officer role")
        await interaction.response.send_message("These commands are for officers only.", ephemeral=True)
        return False

    async def calendar(self, days_back, days_ahead):
        return await asyncio.to_thread(fetch_calendar, days_back, days_ahead)

    # ---------- commands ----------

    @officer.command(name="events", description="Upcoming events: missing rooms, clashes, promo due dates.")
    async def events_cmd(self, interaction: discord.Interaction):
        await interaction.response.defer(ephemeral=True, thinking=True)
        lines = check_events(await self.calendar(0, EVENTS_LOOKAHEAD_DAYS))
        await interaction.followup.send(embed=build_embed("📅 Upcoming events check", lines), ephemeral=True)

    @officer.command(name="vtools", description="Which past events are missing from vTools or have no attendance filed.")
    async def vtools_cmd(self, interaction: discord.Interaction):
        await interaction.response.defer(ephemeral=True, thinking=True)
        lines = await check_vtools(await self.calendar(VTOOLS_LOOKBACK_DAYS, 0))
        await interaction.followup.send(embed=build_embed(
            "🧾 vTools reporting", lines, IEEE_GOLD,
            footer="Checks Google Calendar events only; events made directly in Discord aren't covered."), ephemeral=True)

    @officer.command(name="tabling", description="Upcoming tabling sessions and who's signed up.")
    async def tabling_cmd(self, interaction: discord.Interaction):
        await interaction.response.defer(ephemeral=True, thinking=True)
        lines = await check_tabling()
        await interaction.followup.send(embed=build_embed("🪧 Tabling", lines), ephemeral=True)

    # ---------- reminders ----------

    @reminders.command(name="preview", description="Preview the day-of event reminders for the next 7 days.")
    async def reminders_preview(self, interaction: discord.Interaction):
        cog = self.bot.get_cog("EventReminders")
        lines = cog.preview_lines(interaction.guild) if cog else ["Event reminders aren't loaded."]
        await interaction.response.send_message(embed=build_embed("🔔 Event reminders", lines), ephemeral=True)

    async def scheduled_autocomplete(self, interaction: discord.Interaction, current: str):
        events = sorted((e for e in interaction.guild.scheduled_events if e.status == discord.EventStatus.scheduled),
                        key=lambda e: e.start_time)
        return [app_commands.Choice(name=f"{fmt_day(e.start_time.astimezone(TZ))} · {e.name}"[:100], value=str(e.id))
                for e in events if current.casefold() in e.name.casefold()][:25]

    @reminders.command(name="set", description="Override whether an event gets a reminder and who it pings.")
    @app_commands.describe(event="An upcoming Discord event", send="Send the reminder?", ping="Who the reminder pings")
    @app_commands.autocomplete(event=scheduled_autocomplete)
    @app_commands.choices(
        send=[app_commands.Choice(name="Auto (skip if room is TBA or tentative)", value="auto"),
              app_commands.Choice(name="Always send", value="always"),
              app_commands.Choice(name="Never send", value="never")],
        ping=[app_commands.Choice(name="Auto (from the event name)", value="auto"),
              app_commands.Choice(name="@Workshops", value="workshops"),
              app_commands.Choice(name="@Socials", value="socials"),
              app_commands.Choice(name="@Pro-Dev", value="prodev"),
              app_commands.Choice(name="@everyone", value="everyone"),
              app_commands.Choice(name="No ping", value="none")])
    async def reminders_set(self, interaction: discord.Interaction, event: str,
                            send: app_commands.Choice[str] = None, ping: app_commands.Choice[str] = None):
        cog = self.bot.get_cog("EventReminders")
        target = interaction.guild.get_scheduled_event(int(event)) if event.isdigit() else None
        if cog is None or target is None:
            await interaction.response.send_message("Pick an upcoming event from the list.", ephemeral=True)
            return
        if send is None and ping is None:
            await interaction.response.send_message("Choose `send`, `ping`, or both.", ephemeral=True)
            return
        cog.set_override(target, send=send.value if send else None, ping=ping.value if ping else None)
        preview = [line for line in cog.preview_lines(interaction.guild, days=60)
                   if line.startswith(f"**{target.name}**")]
        await interaction.response.send_message(embed=build_embed(
            "🔔 Reminder updated", preview or ["Saved. The event is more than 60 days out, so there's no preview yet."]),
            ephemeral=True)

    # ---------- deadlines ----------

    @deadlines.command(name="list", description="Chapter deadlines in the next 90 days.")
    async def deadlines_list(self, interaction: discord.Interaction):
        await interaction.response.send_message(embed=build_embed("⏰ Deadlines", check_deadlines(), IEEE_GOLD), ephemeral=True)

    @deadlines.command(name="add", description="Add a deadline to the list and the officer digest.")
    @app_commands.describe(date="YYYY-MM-DD", what="What is due")
    async def deadlines_add(self, interaction: discord.Interaction, date: str, what: str):
        try:
            day = datetime.date.fromisoformat(date.strip())
        except ValueError:
            await interaction.response.send_message("Use the date format YYYY-MM-DD, like 2027-01-15.", ephemeral=True)
            return
        extra = load_json(DEADLINES_EXTRA_FILE, {"added": [], "hidden": []})
        key = deadline_key(day, what.strip())
        extra["hidden"] = [k for k in extra["hidden"] if k != key]
        if not any(deadline_key(datetime.date.fromisoformat(d['date']), d['what']) == key for d in extra["added"]):
            extra["added"].append({"date": day.isoformat(), "what": what.strip()})
        save_json(DEADLINES_EXTRA_FILE, extra)
        await interaction.response.send_message(f"Added: `{day:%a %b %-d, %Y}` {what.strip()}", ephemeral=True)

    async def deadline_autocomplete(self, interaction: discord.Interaction, current: str):
        return [app_commands.Choice(name=f"{day:%b %-d, %Y} · {what}"[:100], value=deadline_key(day, what)[:100])
                for day, what in sorted(load_deadlines()) if current.casefold() in what.casefold()][:25]

    @deadlines.command(name="remove", description="Hide a deadline from the list and the digest.")
    @app_commands.describe(deadline="The deadline to hide")
    @app_commands.autocomplete(deadline=deadline_autocomplete)
    async def deadlines_remove(self, interaction: discord.Interaction, deadline: str):
        match = next(((d, w) for d, w in load_deadlines() if deadline_key(d, w)[:100] == deadline), None)
        if match is None:
            await interaction.response.send_message("Pick a deadline from the list.", ephemeral=True)
            return
        key = deadline_key(*match)
        extra = load_json(DEADLINES_EXTRA_FILE, {"added": [], "hidden": []})
        before = len(extra["added"])
        extra["added"] = [d for d in extra["added"]
                          if deadline_key(datetime.date.fromisoformat(d['date']), d['what']) != key]
        if len(extra["added"]) == before:
            extra["hidden"].append(key)  # a shipped deadline: hide it instead of deleting
        save_json(DEADLINES_EXTRA_FILE, extra)
        await interaction.response.send_message(f"Removed: `{match[0]:%a %b %-d, %Y}` {match[1]}", ephemeral=True)

    # ---------- check-in ----------

    async def pickable_events(self):
        """Recent and upcoming calendar events, cached briefly for autocomplete."""
        fetched, events = self._pick_cache
        now = datetime.datetime.now(TZ)
        if fetched is None or (now - fetched).total_seconds() > 300:
            events = await self.calendar(CHECKIN_PICK_BACK_DAYS, CHECKIN_PICK_AHEAD_DAYS)
            self._pick_cache = (now, events)
        return events

    async def event_autocomplete(self, interaction: discord.Interaction, current: str):
        events = await self.pickable_events()
        return [app_commands.Choice(name=f"{fmt_day(e['start'])} · {e['name']}"[:100], value=e['id'])
                for e in events if current.casefold() in e['name'].casefold()][:25]

    async def form_autocomplete(self, interaction: discord.Interaction, current: str):
        forms = sorted(load_checkins().items(), key=lambda kv: kv[1]['start'], reverse=True)
        return [app_commands.Choice(name=f"{form['day']} · {form['name']}"[:100], value=key)
                for key, form in forms if current.casefold() in form['name'].casefold()][:25]

    @checkin.command(name="create", description="Make (or fetch) the check-in form and QR code for an event.")
    @app_commands.describe(event="The calendar event")
    @app_commands.autocomplete(event=event_autocomplete)
    async def checkin_create(self, interaction: discord.Interaction, event: str):
        await interaction.response.defer(ephemeral=True, thinking=True)
        if not checkin_client.configured():
            await interaction.followup.send("Check-in isn't configured (CHECKIN_URL / CHECKIN_SECRET).", ephemeral=True)
            return
        match = next((e for e in await self.pickable_events() if e['id'] == event), None)
        if match is None:
            await interaction.followup.send("Pick an event from the list.", ephemeral=True)
            return
        day = fmt_day(match['start'])
        try:
            form = await checkin_client.create_form(match['name'], day, match['id'])
        except checkin_client.CheckinError as e:
            await interaction.followup.send(f"The check-in script returned an error: {e}", ephemeral=True)
            return
        checkins = load_checkins()
        checkins[match['id']] = {'formId': form['formId'], 'url': form['url'], 'shortUrl': form['shortUrl'],
                                 'name': match['name'], 'day': day, 'start': match['start'].isoformat()}
        save_checkins(checkins)

        link = form['shortUrl'] or form['url']
        qr = discord.File(io.BytesIO(checkin_client.qr_png(link)), filename="checkin-qr.png")
        embed = create_base_embed(
            title=f"✅ Check-in: {match['name']}",
            description=(f"**Form:** {link}\n\nPut the QR on a slide or print it for the door. "
                         f"Afterwards, run `/officer checkin results` for the vTools numbers."
                         + ("\n\n*This form already existed; same link as before.*" if form.get('existed') else "")),
            color=IEEE_BLUE)
        embed.set_image(url="attachment://checkin-qr.png")
        await interaction.followup.send(embed=embed, file=qr, ephemeral=True)

    async def any_event_autocomplete(self, interaction: discord.Interaction, current: str):
        events = {e['id']: e for e in await self.recent_events() + await self.pickable_events()}.values()
        return [app_commands.Choice(name=f"{fmt_day(e['start'])} · {e['name']}"[:100], value=e['id'])
                for e in sorted(events, key=lambda e: e['start'], reverse=True)
                if current.casefold() in e['name'].casefold()][:25]

    @checkin.command(name="link", description="Use an existing Google Form (owned by the chapter account) as an event's check-in.")
    @app_commands.describe(event="The calendar event", form="The form's edit link or ID")
    @app_commands.autocomplete(event=any_event_autocomplete)
    async def checkin_link(self, interaction: discord.Interaction, event: str, form: str):
        await interaction.response.defer(ephemeral=True, thinking=True)
        events = {e['id']: e for e in await self.recent_events() + await self.pickable_events()}
        match = events.get(event)
        if match is None:
            await interaction.followup.send("Pick an event from the list.", ephemeral=True)
            return
        form_id = checkin_client.parse_form_id(form)
        if not form_id:
            await interaction.followup.send("That doesn't look like a Google Form edit link or ID.", ephemeral=True)
            return
        try:
            info = await checkin_client.form_info(form_id)
        except checkin_client.CheckinError as e:
            await interaction.followup.send(
                f"The chapter account couldn't open that form ({e}). It has to be owned by, or shared with, the chapter account.",
                ephemeral=True)
            return
        checkins = load_checkins()
        checkins[event] = {'formId': info['formId'], 'url': info['url'], 'shortUrl': info['shortUrl'],
                           'name': match['name'], 'day': fmt_day(match['start']),
                           'start': match['start'].isoformat(), 'linked': True}
        save_checkins(checkins)
        await interaction.followup.send(
            f"Linked **{info['title'] or 'the form'}** to {match['name']} ({fmt_day(match['start'])}). "
            "Results count questions mentioning \"email\" and \"IEEE member\".", ephemeral=True)

    @checkin.command(name="results", description="Attendance from an event's check-in form (IEEE vs. guests for vTools).")
    @app_commands.describe(event="An event with a check-in form")
    @app_commands.autocomplete(event=form_autocomplete)
    async def checkin_results(self, interaction: discord.Interaction, event: str):
        await interaction.response.defer(ephemeral=True, thinking=True)
        form = load_checkins().get(event)
        if form is None:
            await interaction.followup.send("No check-in form for that event. Pick one from the list.", ephemeral=True)
            return
        try:
            r = await checkin_client.results(form['formId'])
        except checkin_client.CheckinError as e:
            await interaction.followup.send(f"The check-in script returned an error: {e}", ephemeral=True)
            return
        lines = [
            f"**Checked in:** {r['total']}",
            f"**For vTools:** IEEE attending **{r['ieee']}**, guests **{r['guests']}**",
        ]
        if r['notSure']:
            lines.append(f"({r['notSure']} answered \"Not sure\" about IEEE membership and are counted as guests.)")
        heard = sorted(r['heard'].items(), key=lambda kv: -kv[1])
        if heard:
            lines.append("\n**How they heard about it:**")
            lines += [f"{source}: {count}" for source, count in heard]
        lines.append(f"\nForm: {form['shortUrl'] or form['url']}")
        await interaction.followup.send(embed=build_embed(f"🧾 Check-in results: {form['name']} ({form['day']})", lines, IEEE_GOLD),
                                        ephemeral=True)

    # ---------- wrap-up ----------

    async def recent_events(self):
        fetched, events = self._recent_cache
        now = datetime.datetime.now(TZ)
        if fetched is None or (now - fetched).total_seconds() > 300:
            events = reportable(await self.calendar(WRAPUP_LOOKBACK_DAYS, 0))
            self._recent_cache = (now, events)
        return events

    async def recent_autocomplete(self, interaction: discord.Interaction, current: str):
        events = sorted(await self.recent_events(), key=lambda e: e['start'], reverse=True)
        return [app_commands.Choice(name=f"{fmt_day(e['start'])} · {e['name']}"[:100], value=e['id'])
                for e in events if current.casefold() in e['name'].casefold()][:25]

    @wrapup.command(name="status", description="What's still open for events from the last three weeks.")
    async def wrapup_status(self, interaction: discord.Interaction):
        await interaction.response.defer(ephemeral=True, thinking=True)
        rows = await wrapup_rows(await self.calendar(WRAPUP_LOOKBACK_DAYS, 0))
        await interaction.followup.send(embed=build_embed(
            "🧹 Event wrap-up", format_wrapups(rows), footer="Mark slides or recaps with /officer wrapup mark."), ephemeral=True)

    @wrapup.command(name="mark", description="Mark an event's slides or recap post as done (or not needed).")
    @app_commands.describe(event="A recent event", item="What to mark", state="Done, or not needed for this event")
    @app_commands.autocomplete(event=recent_autocomplete)
    @app_commands.choices(
        item=[app_commands.Choice(name="Slides on GitHub", value="slides"),
              app_commands.Choice(name="Recap post (Instagram/LinkedIn)", value="recap")],
        state=[app_commands.Choice(name="Done", value="done"),
               app_commands.Choice(name="Not needed", value="not needed")])
    async def wrapup_mark(self, interaction: discord.Interaction, event: str,
                          item: app_commands.Choice[str], state: app_commands.Choice[str]):
        match = next((e for e in await self.recent_events() if e['id'] == event), None)
        if match is None:
            await interaction.response.send_message("Pick an event from the list.", ephemeral=True)
            return
        marks = load_wrapups()
        marks.setdefault(event, {})[item.value] = state.value
        save_wrapups(marks)
        await interaction.response.send_message(
            f"Marked **{item.name}** as **{state.name.lower()}** for {match['name']} ({fmt_day(match['start'])}).", ephemeral=True)

    async def cached_repos(self):
        fetched, repos = self._repo_cache
        now = datetime.datetime.now(TZ)
        if fetched is None or (now - fetched).total_seconds() > 600:
            async with aiohttp.ClientSession() as session:
                repos = await fetch_repos(session)
            self._repo_cache = (now, repos)
        return repos

    async def repo_autocomplete(self, interaction: discord.Interaction, current: str):
        choices = [app_commands.Choice(name="auto (detect it again)", value="auto")]
        try:
            repos = sorted(await self.cached_repos(), key=lambda r: r['created_at'], reverse=True)
        except Exception:
            repos = []
        choices += [app_commands.Choice(name=r['name'][:100], value=r['name'][:100])
                    for r in repos if current.casefold() in r['name'].casefold()]
        return choices[:25]

    @wrapup.command(name="slides", description="Link an event to the repo (or any link) holding its slides.")
    @app_commands.describe(event="A recent event", repo="An IEEECS-UCF repo, any https link, or auto")
    @app_commands.autocomplete(event=recent_autocomplete, repo=repo_autocomplete)
    async def wrapup_slides(self, interaction: discord.Interaction, event: str, repo: str):
        await interaction.response.defer(ephemeral=True, thinking=True)
        match = next((e for e in await self.recent_events() if e['id'] == event), None)
        if match is None:
            await interaction.followup.send("Pick an event from the list.", ephemeral=True)
            return
        repo = repo.strip()
        marks = load_wrapups()
        mine = marks.setdefault(event, {})
        if repo == "auto":
            mine.pop('slides_link', None)
            note = "Cleared; slides will be detected automatically again."
        else:
            known = next((r for r in await self.cached_repos() if r['name'].casefold() == repo.casefold()), None)
            if known:
                mine['slides_link'] = {'name': known['name'], 'url': known['html_url']}
            elif repo.startswith("https://"):
                mine['slides_link'] = {'name': repo.rstrip('/').rsplit('/', 1)[-1] or repo, 'url': repo}
            else:
                await interaction.followup.send(f"`{repo}` isn't an {GITHUB_ORG} repo or an https link.", ephemeral=True)
                return
            note = f"Linked slides: {mine['slides_link']['url']}"
        save_wrapups(marks)
        await interaction.followup.send(f"**{match['name']}** ({fmt_day(match['start'])}): {note}", ephemeral=True)

    async def vtools_choice_autocomplete(self, interaction: discord.Interaction, current: str):
        options = [app_commands.Choice(name="auto (match it again)", value="auto"),
                   app_commands.Choice(name="not needed (don't report this event)", value=SKIPPED)]
        if parse_event_id(current):
            options.insert(0, app_commands.Choice(name=f"vTools event {parse_event_id(current)}", value=current[:100]))
        return options

    @wrapup.command(name="vtools", description="Link an event to its vTools event, or mark it as not reported.")
    @app_commands.describe(event="A recent event", vtools="vTools event ID or link (events.vtools.ieee.org/m/…), auto, or not needed")
    @app_commands.autocomplete(event=recent_autocomplete, vtools=vtools_choice_autocomplete)
    async def wrapup_vtools(self, interaction: discord.Interaction, event: str, vtools: str):
        await interaction.response.defer(ephemeral=True, thinking=True)
        match = next((e for e in await self.recent_events() if e['id'] == event), None)
        if match is None:
            await interaction.followup.send("Pick an event from the list.", ephemeral=True)
            return
        marks = load_wrapups()
        mine = marks.setdefault(event, {})
        if vtools == "auto":
            mine.pop('vtools', None)
            note = "Cleared; it will be matched automatically again."
        elif vtools == SKIPPED:
            mine['vtools'] = SKIPPED
            note = "Marked as not needing a vTools report."
        else:
            event_id = parse_event_id(vtools)
            record = None
            if event_id:
                async with aiohttp.ClientSession() as session:
                    record = await fetch_event(session, event_id)
            if record is None:
                await interaction.followup.send(f"Couldn't find vTools event `{vtools}`.", ephemeral=True)
                return
            mine['vtools'] = {'id': event_id, 'link': record['link']}
            filed = record.get('ieee-attending')
            note = (f"Linked to **{record['title']}** <{record['link']}>. "
                    + (f"Attendance filed: {filed} IEEE, {record.get('guests-attending') or 0} guests."
                       if filed is not None else "No attendance filed yet."))
        save_wrapups(marks)
        await interaction.followup.send(f"**{match['name']}** ({fmt_day(match['start'])}): {note}", ephemeral=True)

    # ---------- listings ----------

    async def window_events(self):
        return await self.calendar(CROSSCHECK_BACK_DAYS, CROSSCHECK_AHEAD_DAYS)

    @listings.command(name="status", description="Compare KnightConnect and vTools listings with the calendar.")
    async def listings_status(self, interaction: discord.Interaction):
        await interaction.response.defer(ephemeral=True, thinking=True)
        rows = await crosscheck_rows(await self.window_events())
        await interaction.followup.send(embed=build_embed(
            "🔗 Listings vs. calendar", format_crosscheck(rows),
            footer="Fix a wrong match with /officer listings link."), ephemeral=True)

    async def window_autocomplete(self, interaction: discord.Interaction, current: str):
        events = sorted(await self.window_events(), key=lambda e: e['start'])
        return [app_commands.Choice(name=f"{fmt_day(e['start'])} · {e['name']}"[:100], value=e['id'])
                for e in events if current.casefold() in e['name'].casefold()][:25]

    async def listing_choice_autocomplete(self, interaction: discord.Interaction, current: str):
        options = [app_commands.Choice(name="auto (match it again)", value="auto"),
                   app_commands.Choice(name="not needed (no listing for this event)", value=SKIPPED)]
        if parse_event_id(current):
            options.insert(0, app_commands.Choice(name=f"Listing {parse_event_id(current)}", value=current[:100]))
        return options

    @listings.command(name="copy", description="Fields for an event, laid out for KnightConnect's or vTools' create form.")
    @app_commands.describe(platform="Which site's form", event="The calendar event")
    @app_commands.choices(platform=[app_commands.Choice(name="KnightConnect", value="knightconnect"),
                                    app_commands.Choice(name="vTools", value="vtools")])
    @app_commands.autocomplete(event=window_autocomplete)
    async def listings_copy(self, interaction: discord.Interaction, platform: app_commands.Choice[str], event: str):
        await interaction.response.defer(ephemeral=True, thinking=True)
        match = next((e for e in await self.window_events() if e['id'] == event), None)
        if match is None:
            await interaction.followup.send("Pick an event from the list.", ephemeral=True)
            return
        embed = create_base_embed(title=f"📋 {platform.name}: {match['name']}",
                                  description=event_text.copy_block(match, platform.value), color=IEEE_BLUE)
        files = []
        sync = self.bot.get_cog("CalendarSync")
        if platform.value == "knightconnect" and match.get('flyer') and sync:
            image = await sync.download_flyer(match['flyer'])
            if image:
                files.append(discord.File(io.BytesIO(image), filename="flyer.png"))
                embed.set_image(url="attachment://flyer.png")
        await interaction.followup.send(embed=embed, files=files, ephemeral=True)

    @listings.command(name="link", description="Point an event at its KnightConnect or vTools listing, or mark it not needed.")
    @app_commands.describe(platform="Which site", event="The calendar event",
                           listing="Listing ID or link, auto, or not needed")
    @app_commands.choices(platform=[app_commands.Choice(name="KnightConnect", value="knightconnect"),
                                    app_commands.Choice(name="vTools", value="vtools")])
    @app_commands.autocomplete(event=window_autocomplete, listing=listing_choice_autocomplete)
    async def listings_link(self, interaction: discord.Interaction, platform: app_commands.Choice[str],
                            event: str, listing: str):
        await interaction.response.defer(ephemeral=True, thinking=True)
        match = next((e for e in await self.window_events() if e['id'] == event), None)
        if match is None:
            await interaction.followup.send("Pick an event from the list.", ephemeral=True)
            return
        marks = load_wrapups()
        mine = marks.setdefault(event, {})
        if listing == "auto":
            mine.pop(platform.value, None)
            note = "Cleared; it will be matched automatically again."
        elif listing == SKIPPED:
            mine[platform.value] = SKIPPED
            note = f"Marked as not needing a {platform.name} listing."
        else:
            listing_id = parse_event_id(listing)
            async with aiohttp.ClientSession() as session:
                if platform.value == "knightconnect":
                    record = await kc.fetch_event(session, listing_id) if listing_id else None
                    title = record and record['name']
                else:
                    record = await fetch_event(session, listing_id) if listing_id else None
                    title = record and record['title']
            if record is None:
                await interaction.followup.send(f"Couldn't find {platform.name} listing `{listing}`.", ephemeral=True)
                return
            mine[platform.value] = {'id': listing_id, 'link': record['link']}
            note = f"Linked to **{title}** <{record['link']}>."
        save_wrapups(marks)
        await interaction.followup.send(f"**{match['name']}** ({fmt_day(match['start'])}), {platform.name}: {note}", ephemeral=True)

    # ---------- announcement drafts ----------

    def discord_link(self, gid):
        sync = self.bot.get_cog("CalendarSync")
        did = sync.state["event_map"].get(gid) if sync else None
        return f"https://discord.com/events/{os.getenv('GUILD_ID')}/{did}" if did else ""

    @officer.command(name="draft", description="Draft the #announcements post and Instagram caption for an event.")
    @app_commands.describe(event="The calendar event")
    @app_commands.autocomplete(event=window_autocomplete)
    async def draft_cmd(self, interaction: discord.Interaction, event: str):
        await interaction.response.defer(ephemeral=True, thinking=True)
        match = next((e for e in await self.window_events() if e['id'] == event), None)
        if match is None:
            await interaction.followup.send("Pick an event from the list.", ephemeral=True)
            return
        role_key = event_text.kind(match)
        role_id = os.getenv({'workshops': 'REMINDER_ROLE_WORKSHOPS', 'socials': 'REMINDER_ROLE_SOCIALS',
                             'prodev': 'REMINDER_ROLE_PRODEV'}.get(role_key, ''), '')
        draft = event_text.announcement_draft(match, self.discord_link(match['id']),
                                              f"<@&{role_id}>" if role_id else None)
        caption = event_text.instagram_caption(match)
        text = (f"**#announcements draft** (edit before posting; mentions only ping when posted)\n```md\n{draft}\n```"
                f"\n**Instagram caption**\n```\n{caption}\n```")
        channel_id = os.getenv('ANNOUNCE_CHANNEL_ID') or os.getenv('REMINDER_CHANNEL_ID')
        view = DraftView(draft, int(channel_id)) if channel_id else None
        await interaction.followup.send(text[:2000], view=view, ephemeral=True)

    # ---------- owners and nudges ----------

    @officer.command(name="owner", description="Set who owns an event (they get its nudges). Leave user empty to clear.")
    @app_commands.describe(event="The calendar event", user="The officer responsible")
    @app_commands.autocomplete(event=window_autocomplete)
    async def owner_cmd(self, interaction: discord.Interaction, event: str, user: discord.Member = None):
        match = next((e for e in await self.window_events() if e['id'] == event), None)
        if match is None:
            await interaction.response.send_message("Pick an event from the list.", ephemeral=True)
            return
        owners = load_json(OWNERS_FILE, {})
        if user is None:
            owners.pop(event, None)
            default, _ = owner_of(match, owners)
            note = f"Owner cleared; back to the default (<@{default}>)." if default else "Owner cleared."
        else:
            owners[event] = user.id
            note = f"Owner: {user.mention}"
        save_json(OWNERS_FILE, owners)
        await interaction.response.send_message(f"**{match['name']}** ({fmt_day(match['start'])}): {note}",
                                                ephemeral=True, allowed_mentions=discord.AllowedMentions.none())

    async def nudge_events(self):
        return await self.calendar(WRAPUP_LOOKBACK_DAYS, CROSSCHECK_AHEAD_DAYS)

    @officer.command(name="nudges", description="Preview the nudges event owners would get today.")
    async def nudges_cmd(self, interaction: discord.Interaction):
        await interaction.response.defer(ephemeral=True, thinking=True)
        events = await self.nudge_events()
        by_id = {e['id']: e for e in events}
        owners = load_json(OWNERS_FILE, {})
        state = load_json(NUDGES_FILE, {"sent": {}, "threads": {}})
        due = due_nudges(await open_items(events), state["sent"], datetime.datetime.now(TZ).date())
        lines = []
        for gid, items in sorted(due.items(), key=lambda kv: by_id[kv[0]]['start']):
            e = by_id[gid]
            owner_id, _ = owner_of(e, owners)
            who = f"<@{owner_id}>" if owner_id else "**no owner** (`/officer owner`)"
            lines.append(f"`{fmt_day(e['start'])}` **{e['name']}** → {who}\n" + "\n".join(f"  • {t}" for _, t in items))
        status = "on" if self.nudge_channel_id else "off (NUDGE_CHANNEL_ID not set)"
        await interaction.followup.send(embed=build_embed(
            "📌 Nudges due today", lines or ["Nothing due."], footer=f"Daily nudges are {status}."), ephemeral=True)

    @tasks.loop(minutes=30)
    async def daily_nudges(self):
        # A raised exception would stop tasks.loop for good, so catch everything
        try:
            await self.send_nudges_if_due()
        except Exception:
            print("Event nudges failed:")
            traceback.print_exc()

    @daily_nudges.before_loop
    async def before_nudges(self):
        await self.bot.wait_until_ready()

    async def send_nudges_if_due(self, force=False):
        now = datetime.datetime.now(TZ)
        state = load_json(NUDGES_FILE, {"sent": {}, "threads": {}, "day": None})
        if not force and (now.hour < NUDGE_HOUR or state.get("day") == now.date().isoformat()):
            return
        channel = self.bot.get_channel(self.nudge_channel_id)
        if channel is None:
            print("Error: Could not find the nudge channel.")
            return
        events = await self.nudge_events()
        by_id = {e['id']: e for e in events}
        owners = load_json(OWNERS_FILE, {})
        due = due_nudges(await open_items(events), state["sent"], now.date())
        for gid, items in due.items():
            e = by_id[gid]
            owner, _ = owner_of(e, owners)
            text = ((f"<@{owner}> " if owner else "No owner yet (`/officer owner`). ")
                    + f"Still open for **{e['name']}** ({fmt_day(e['start'])}):\n"
                    + "\n".join(f"• {t}" for _, t in items))
            allowed = discord.AllowedMentions(everyone=False, roles=False,
                                              users=[discord.Object(int(owner))] if owner else False)
            thread = await self.event_thread(channel, e, state)
            await thread.send(text, allowed_mentions=allowed)
            for key, _ in items:
                state["sent"][f"{gid}|{key}"] = now.date().isoformat()
            save_json(NUDGES_FILE, state)
            print(f"Nudged {e['name']}: {', '.join(k for k, _ in items)}")
        state["day"] = now.date().isoformat()
        save_json(NUDGES_FILE, state)

    async def event_thread(self, channel, event, state):
        """The event's own thread in the nudge channel, created on first use."""
        thread_id = state["threads"].get(event['id'])
        if thread_id:
            thread = self.bot.get_channel(int(thread_id))
            if thread is None:
                try:
                    thread = await self.bot.fetch_channel(int(thread_id))
                except discord.HTTPException:
                    thread = None
            if thread is not None:
                if getattr(thread, 'archived', False):
                    await thread.edit(archived=False)
                return thread
        name = f"{event['name']} ({fmt_day(event['start'])})"[:100]
        if isinstance(channel, discord.ForumChannel):
            created = await channel.create_thread(name=name, content=f"Tracking **{event['name']}**: open items get posted here.")
            thread = created.thread
        else:
            starter = await channel.send(f"📌 **{event['name']}** ({fmt_day(event['start'])}): open items go in this thread.")
            thread = await starter.create_thread(name=name)
        state["threads"][event['id']] = thread.id
        save_json(NUDGES_FILE, state)
        return thread

    # ---------- volunteer board ----------

    def volunteer_channel(self):
        channel_id = os.getenv('VOLUNTEER_CHANNEL_ID')
        return self.bot.get_channel(int(channel_id)) if channel_id else None

    async def post_task(self, task):
        channel = self.volunteer_channel()
        message = await channel.send(embed=volunteer.task_embed(task), view=volunteer.task_view(task),
                                     allowed_mentions=discord.AllowedMentions.none())
        task["channel_id"], task["message_id"] = channel.id, message.id
        return message

    @task.command(name="add", description="Post a task members can claim.")
    @app_commands.describe(title="What needs doing", when="When (free text)", details="Anything else", spots="How many people")
    async def task_add(self, interaction: discord.Interaction, title: str, when: str = None,
                       details: str = None, spots: app_commands.Range[int, 1, 20] = 1):
        if self.volunteer_channel() is None:
            await interaction.response.send_message("The volunteer board is off (VOLUNTEER_CHANNEL_ID not set).", ephemeral=True)
            return
        await interaction.response.defer(ephemeral=True, thinking=True)
        tasks = volunteer.load_tasks()
        task = volunteer.new_task(title, when, details, spots, interaction.user.id)
        message = await self.post_task(task)
        tasks[task["id"]] = task
        volunteer.save_tasks(tasks)
        await interaction.followup.send(f"Posted: {message.jump_url}", ephemeral=True)

    @task.command(name="tabling", description="Post claimable tasks for understaffed tabling sessions in the next two weeks.")
    async def task_tabling(self, interaction: discord.Interaction):
        if self.volunteer_channel() is None or not TABLING_SHEET_ID:
            await interaction.response.send_message("Needs VOLUNTEER_CHANNEL_ID and TABLING_SHEET_ID set.", ephemeral=True)
            return
        await interaction.response.defer(ephemeral=True, thinking=True)
        async with aiohttp.ClientSession() as session:
            rows = await fetch_tabling(session)
        tasks = volunteer.load_tasks()
        existing = {t.get("key") for t in tasks.values()}
        today = datetime.datetime.now(TZ).date()
        posted = []
        for r in rows:
            try:
                day = datetime.date.fromisoformat(r.get('Date', '').strip())
            except ValueError:
                continue
            filled = [r.get(k, '').strip() for k in ('Volunteer 1', 'Volunteer 2') if r.get(k, '').strip()]
            key = f"tabling|{day.isoformat()}|{r.get('Time', '').strip()}"
            if not 0 <= (day - today).days <= 14 or len(filled) >= 2 or key in existing:
                continue
            task = volunteer.new_task(
                f"Table: {r.get('Event Name', '').strip()} ({day:%a %b %-d})",
                f"{r.get('Time', '').strip()} · {r.get('Location', '').strip()}",
                (r.get('Notes', '').strip() + ("\nAlready signed up on the sheet: " + ", ".join(filled) if filled else "")).strip()
                + "\nAlso add your name to the Tabling Signup sheet.",
                2 - len(filled), interaction.user.id, key=key)
            message = await self.post_task(task)
            tasks[task["id"]] = task
            posted.append(f"{task['title']}: {message.jump_url}")
        volunteer.save_tasks(tasks)
        await interaction.followup.send("\n".join(posted) or "Every session in the next two weeks is staffed or already posted.",
                                        ephemeral=True)

    async def open_task_autocomplete(self, interaction: discord.Interaction, current: str):
        return [app_commands.Choice(name=t["title"][:100], value=t["id"])
                for t in volunteer.load_tasks().values() if t["open"] and current.casefold() in t["title"].casefold()][:25]

    @task.command(name="list", description="Open tasks and who claimed them.")
    async def task_list(self, interaction: discord.Interaction):
        lines = [f"**{t['title']}** ({len(t['claimed'])}/{t['slots']}): "
                 + (", ".join(f"<@{u}>" for u in t["claimed"]) or "nobody yet")
                 for t in volunteer.load_tasks().values() if t["open"]]
        await interaction.response.send_message(embed=build_embed("🙋 Open tasks", lines or ["No open tasks."]), ephemeral=True)

    @task.command(name="close", description="Close a task (its buttons go away).")
    @app_commands.describe(task="The task")
    @app_commands.autocomplete(task=open_task_autocomplete)
    async def task_close(self, interaction: discord.Interaction, task: str):
        tasks = volunteer.load_tasks()
        found = tasks.get(task)
        if found is None:
            await interaction.response.send_message("Pick a task from the list.", ephemeral=True)
            return
        found["open"] = False
        volunteer.save_tasks(tasks)
        channel = self.bot.get_channel(found["channel_id"]) if found["channel_id"] else None
        if channel:
            try:
                message = await channel.fetch_message(found["message_id"])
                await message.edit(embed=volunteer.task_embed(found), view=None)
            except discord.HTTPException as e:
                print(f"Could not update closed task message: {e}")
        await interaction.response.send_message(f"Closed **{found['title']}**.", ephemeral=True)

    # ---------- officer digest (off unless configured) ----------

    @tasks.loop(minutes=30)
    async def officer_digest(self):
        # A raised exception would stop tasks.loop for good, so catch everything
        try:
            await self.post_digest_if_due()
        except Exception:
            print("Officer digest failed:")
            traceback.print_exc()

    @officer_digest.before_loop
    async def before_digest(self):
        await self.bot.wait_until_ready()

    async def post_digest_if_due(self):
        now = datetime.datetime.now(TZ)
        state = load_json(DIGEST_STATE_FILE, {})
        if not digest_due(now, state.get('day')):
            return
        channel = self.bot.get_channel(self.digest_channel_id)
        if channel is None:
            print("Error: Could not find officer digest channel.")
            return
        events = await self.calendar(VTOOLS_LOOKBACK_DAYS, EVENTS_LOOKAHEAD_DAYS)
        embeds = [
            build_embed("📅 Upcoming events check", check_events(events)),
            build_embed("🧾 vTools reporting", [l for l in await check_vtools(events) if not l.startswith("✅")] or ["All caught up."], IEEE_GOLD),
            build_embed("🔗 Listings vs. calendar", format_crosscheck(await crosscheck_rows(events), problems_only=True)),
            build_embed("🧹 Event wrap-up", format_wrapups(await wrapup_rows(events), open_only=True)),
            build_embed("🪧 Tabling", await check_tabling()),
            build_embed("⏰ Deadlines", check_deadlines(), IEEE_GOLD),
        ]
        role = os.getenv('OFFICER_DIGEST_ROLE_ID')
        content = f"<@&{role}> **Officer digest**" if role else "**Officer digest**"
        for i, batch in enumerate(split_embeds(embeds)):
            # Only the first message pings
            await channel.send(content=content if i == 0 else None, embeds=batch, allowed_mentions=discord.AllowedMentions(
                everyone=False, users=False, roles=[discord.Object(int(role))] if role and i == 0 else False))
        save_json(DIGEST_STATE_FILE, {'day': now.date().isoformat()})


async def setup(bot):
    await bot.add_cog(OfficerTools(bot))
