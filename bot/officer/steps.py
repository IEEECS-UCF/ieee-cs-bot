import os
import types
import datetime
import difflib
from dataclasses import dataclass, field

from dateutil import parser

from bot.utils import event_text
from bot.utils.event_text import same_place
from bot.cogs import event_reminders
from bot.officer.data import SKIPPED
from bot.officer.sources import TZ, plain, vtools_location

# The status model: every event moves through four phases, each a fixed list
# of steps with a due date relative to the event. The board, event card,
# digest and nudges are all drawn from these steps.

DONE, TODO, OVERDUE, WARN, LATER, SKIP = "✅", "⬜", "🔴", "⚠️", "⏳", "➖"
PROBLEMS = (TODO, OVERDUE, WARN)
LEGEND = "✅ done · ⬜ to do · 🔴 overdue · ⚠️ mismatch · ⏳ not yet · ➖ not needed"
PHASES = ("Plan", "Promote", "Run", "Wrap up")
ACTIVE_DAYS = 7  # a step shows as "to do" this many days before it's due

# Default owner by kind of event ("workshops:<id>,other:<id>"); "other" covers GBMs, TechX...
DEFAULT_OWNERS = dict(pair.split(":", 1) for pair in os.getenv('DEFAULT_OWNERS', '').split(",") if ":" in pair)
# Steps only officers can mark (announcement, recap) aren't tracked for events before this date
TRACKING_START = datetime.date.fromisoformat(os.getenv('TRACKING_START', '2026-10-30'))


@dataclass
class Step:
    key: str
    phase: str
    label: str
    state: str
    detail: str = ""
    actions: list = field(default_factory=list)

    def line(self):
        return f"{self.state} **{self.label}**" + (f": {self.detail}" if self.detail else "")


def owner_of(event, record):
    """(user id or None, set by hand?)"""
    if record.get('owner'):
        return int(record['owner']), True
    default = DEFAULT_OWNERS.get(event_text.kind(event) or 'other')
    return (int(default), False) if default else (None, False)

def no_room(location):
    return (location or '').strip().casefold() in ('', 'tba', 'tbd')

def room_state(event, record):
    """(status, room): 'none', 'requested', 'confirmed' or 'denied'.

    A room typed into the calendar counts as confirmed, and so does a request
    whose room has since shown up there.
    """
    room = record.get('room')
    has_room = not no_room(event['location'])
    if room:
        if room['status'] == 'requested' and has_room and same_place(room['room'], event['location']):
            return 'confirmed', event['location']
        return room['status'], room.get('room') or event['location']
    return ('confirmed', event['location']) if has_room else ('none', None)

def booked_room(event, record):
    """The room an event holds or has asked for (a denied request holds nothing)."""
    status, room = room_state(event, record)
    return room if status in ('requested', 'confirmed') else None

def clashes(snap, start, end, room=None, exclude=None):
    """[(event, same room?)] for events overlapping start-end, other than exclude.

    The one clash check: the New event preview, Edit details and room requests all use it.
    """
    found = []
    for other in snap.events:
        if other['id'] == exclude or not (other['start'] < end and start < other['end']):
            continue
        theirs = booked_room(other, snap.store.get(other['id'], {}))
        found.append((other, bool(room and theirs and same_place(room, theirs))))
    return found

def timed(today, due, active_from=None):
    """LATER before the step is active, TODO until due, OVERDUE after."""
    active_from = active_from or due - datetime.timedelta(days=ACTIVE_DAYS)
    if today < active_from:
        return LATER
    return TODO if today <= due else OVERDUE

def listing_diffs(event, start, location, title):
    diffs = []
    if abs((start - event['start']).total_seconds()) > 60:
        diffs.append(f"time {start:%a %b %-d %-I:%M %p} vs calendar {event['start']:%-I:%M %p}")
    if location and not no_room(event['location']) and not same_place(location, event['location']):
        diffs.append(f"room {location} vs calendar {event['location']}")
    ours, theirs = plain(event['name']), plain(title)
    if ours not in theirs and theirs not in ours and difflib.SequenceMatcher(None, ours, theirs).ratio() < 0.4:
        diffs.append(f'title "{title}"')
    return diffs


def plan_steps(e, record, marks, today, start):
    steps = []
    owner, by_hand = owner_of(e, record)
    steps.append(Step("owner", "Plan", "Owner",
                      DONE if owner else timed(today, start - datetime.timedelta(days=28)),
                      (f"<@{owner}>" + ("" if by_hand else " (default)")) if owner else "nobody yet", ["owner"]))

    status, room = room_state(e, record)
    request = record.get('room', {})
    if status == 'confirmed':
        state, detail = DONE, room
    elif status == 'denied':
        state, detail = OVERDUE, f"{room} was denied; request another"
    elif status == 'requested':
        since = datetime.date.fromisoformat(request['since'])
        waiting = (today - since).days
        late = waiting >= 7 or today > start - datetime.timedelta(days=14)
        state, detail = (OVERDUE if late else TODO), f"{room} requested {since:%b %-d}, no answer yet"
    else:
        state, detail = timed(today, start - datetime.timedelta(days=28)), "not requested yet"
    steps.append(Step("room", "Plan", "Room", state, detail, ["room"]))

    has_text = bool(event_text.plain_description(e))
    steps.append(Step("description", "Plan", "Description",
                      SKIP if marks.get('description') == SKIPPED else
                      DONE if has_text else timed(today, start - datetime.timedelta(days=14)),
                      "" if has_text else "empty in the calendar", ["mark:description"]))
    steps.append(Step("flyer", "Plan", "Flyer",
                      SKIP if marks.get('flyer') == SKIPPED else
                      DONE if e.get('flyer') else timed(today, start - datetime.timedelta(days=14)),
                      "" if e.get('flyer') else 'add a "Flyer: <link>" line to the calendar event', ["mark:flyer"]))
    return steps

def listing_step(key, label, e, match, today, start, start_key, title_key, location_of):
    actions = [f"copy:{key}", f"link:{key}"]
    if match == SKIPPED:
        return Step(key, "Promote", label, SKIP, "not needed", actions)
    if match is None:
        return Step(key, "Promote", label, timed(today, start - datetime.timedelta(days=21)), "not listed", actions)
    diffs = listing_diffs(e, parser.isoparse(match[start_key]).astimezone(TZ), location_of(match), match[title_key])
    linked = " (linked by hand)" if match.get('linked') else ""
    if diffs:
        return Step(key, "Promote", label, WARN, "; ".join(diffs) + f" · <{match['link']}>", actions)
    return Step(key, "Promote", label, DONE, f"listed{linked} · <{match['link']}>", actions)

def promote_steps(e, record, marks, snap, today, start):
    steps = [
        listing_step("knightconnect", "KnightConnect", e, snap.knightconnect.get(e['id']), today, start,
                     'startsOn', 'name', lambda m: m.get('location')),
        listing_step("vtools", "vTools", e, snap.vtools.get(e['id']), today, start,
                     'start-time', 'title', vtools_location),
    ]
    announced = marks.get('announcement')
    steps.append(Step("announcement", "Promote", "Announcement",
                      DONE if announced == 'done' else SKIP if announced == SKIPPED
                      else timed(today, start - datetime.timedelta(days=7)),
                      "posted" if announced == 'done' else "", ["draft", "mark:announcement"]))
    return steps

def reminder_step(e, record, snap, now):
    did = snap.discord_ids.get(e['id'])
    override = record.get('reminder')
    fake = types.SimpleNamespace(name=e['name'], location=e['location'])
    send, why = event_reminders.should_send(fake, override)
    kind, role_id = event_reminders.resolve_ping(fake, override)
    who = f"<@&{role_id}>" if kind == 'role' else "@everyone" if kind == 'everyone' else "no ping"
    if did and int(did) in snap.reminders_sent:
        return Step("reminder", "Run", "Day-of reminder", DONE, "posted", ["reminder"])
    if not send:
        state = SKIP if (override or {}).get('send') == 'never' else (WARN if (e['start'] - now).days <= 3 else LATER)
        return Step("reminder", "Run", "Day-of reminder", state, why, ["reminder"])
    return Step("reminder", "Run", "Day-of reminder", LATER,
                f"posts {e['start']:%a} at {event_reminders.REMINDER_HOUR}:00, {who}", ["reminder"])

def run_steps(e, record, marks, snap, today, start, now):
    steps = []
    form = record.get('checkin')
    counts = snap.checkins.get(e['id'], {})
    if form:
        detail = (f"{counts['total']} checked in ({counts['ieee']} IEEE)" if 'total' in counts
                  else "form ready" if not counts else "results unreadable")
        steps.append(Step("checkin", "Run", "Check-in", DONE, detail, ["checkin"]))
    else:
        steps.append(Step("checkin", "Run", "Check-in",
                          SKIP if marks.get('checkin') == SKIPPED
                          else timed(today, start - datetime.timedelta(days=2), start - datetime.timedelta(days=7)),
                          "no form yet", ["checkin", "mark:checkin"]))
    steps.append(reminder_step(e, record, snap, now))
    return steps

def wrap_steps(e, record, marks, snap, today, end, now):
    after = "after the event"
    if now < e['end']:
        return [Step(k, "Wrap up", label, LATER, after, acts) for k, label, acts in
                (("attendance", "Attendance in vTools", ["attendance"]), ("slides", "Slides", ["slides", "mark:slides"]),
                 ("recap", "Recap post", ["mark:recap"]))]
    steps = []
    record_vt = snap.vtools.get(e['id'])
    counts = snap.checkins.get(e['id'], {})
    check = f" · check-in says {counts['ieee']} IEEE, {counts['guests']} guests" if 'total' in counts else ""
    if record_vt == SKIPPED:
        steps.append(Step("attendance", "Wrap up", "Attendance in vTools", SKIP, "not reported", ["attendance"]))
    elif record_vt and record_vt.get('ieee-attending') is not None:
        ieee, guests = record_vt['ieee-attending'], record_vt.get('guests-attending') or 0
        steps.append(Step("attendance", "Wrap up", "Attendance in vTools", DONE, f"{ieee} IEEE, {guests} guests", ["attendance"]))
    else:
        where = "file the report" if record_vt else "create the vTools event, then file the report"
        steps.append(Step("attendance", "Wrap up", "Attendance in vTools",
                          timed(today, end + datetime.timedelta(days=3), end), where + check, ["attendance"]))

    slides = snap.slides.get(e['id'])
    # Only workshops put slides on GitHub; GBMs, socials and TechX don't, unless someone links them
    if (event_text.kind(e) != 'workshops' and not slides) or marks.get('slides') == SKIPPED:
        steps.append(Step("slides", "Wrap up", "Slides", SKIP, "not needed", ["slides", "mark:slides"]))
    elif slides or marks.get('slides') == 'done':
        detail = slides['name'] + (" (linked by hand)" if slides.get('linked') else "") if slides else "marked done"
        steps.append(Step("slides", "Wrap up", "Slides", DONE, detail, ["slides", "mark:slides"]))
    else:
        steps.append(Step("slides", "Wrap up", "Slides", timed(today, end + datetime.timedelta(days=7), end),
                          f"nothing in {os.getenv('GITHUB_ORG', 'IEEECS-UCF')} yet", ["slides", "mark:slides"]))
    recap = marks.get('recap')
    if end < TRACKING_START and not recap:
        steps.append(Step("recap", "Wrap up", "Recap post", SKIP, "before tracking", ["mark:recap"]))
        return steps
    steps.append(Step("recap", "Wrap up", "Recap post",
                      DONE if recap == 'done' else SKIP if recap == SKIPPED
                      else timed(today, end + datetime.timedelta(days=7), end),
                      "", ["mark:recap"]))
    return steps


def event_steps(e, snap, now=None):
    """All steps for one event, in phase order."""
    now = now or datetime.datetime.now(TZ)
    today, start, end = now.date(), e['start'].date(), e['end'].date()
    record = snap.store.get(e['id'], {})
    marks = record.get('marks', {})
    steps = (plan_steps(e, record, marks, today, start)
            + promote_steps(e, record, marks, snap, today, start)
            + run_steps(e, record, marks, snap, today, start, now)
            + wrap_steps(e, record, marks, snap, today, end, now))
    if now >= e['start']:
        # Before-the-event steps can't be fixed once it has started
        for step in steps:
            if step.phase != "Wrap up" and step.state not in (DONE, SKIP):
                step.state, step.detail = SKIP, "missed"
    return steps

def is_open(e, steps, now=None):
    """On the board: upcoming, or past with something still open."""
    now = now or datetime.datetime.now(TZ)
    return e['end'] > now or any(s.state in PROBLEMS for s in steps)

def problems(steps):
    return [s for s in steps if s.state in PROBLEMS]
