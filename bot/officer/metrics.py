import io
import csv
import asyncio
import datetime
from collections import Counter

import aiohttp

from bot.utils import event_text
from bot.utils.formatting import IEEE_BLUE, IEEE_GOLD
from bot.officer import members
from bot.officer.data import EventStore, load_json, save_json
from bot.officer.render import build_embed
from bot.officer.sources import TZ, Snapshot, fetch_calendar, _vtools, _checkins, _slides
from bot.officer.steps import event_steps, DONE, SKIP

# The monthly report: what the chapter ran, who came, and how well each event
# was wrapped up. Attendance comes from vTools when the report is filed there,
# otherwise from the event's check-in form. Each finished month's numbers are
# kept in METRICS_FILE so the next report can compare against them.
#
#   {"months": {"2026-10": {...summary}}, "posted": "2026-10"}
METRICS_FILE = "data/metrics.json"
KIND_NAMES = {"workshops": "Workshops", "socials": "Socials", "prodev": "Professional", None: "GBMs & other"}
FOLLOW_UP = (("attendance", "Attendance in vTools"), ("slides", "Slides on GitHub"), ("recap", "Recap posted"))


def month_span(year, month):
    start = datetime.datetime(year, month, 1, tzinfo=TZ)
    end = datetime.datetime(year + (month == 12), month % 12 + 1, 1, tzinfo=TZ)
    return start, end

def previous_month(year, month):
    return (year - 1, 12) if month == 1 else (year, month - 1)

def month_key(year, month):
    return f"{year}-{month:02d}"

def month_label(year, month):
    return datetime.date(year, month, 1).strftime("%B %Y")

def attendance(e, snap):
    """(ieee, guests, where it came from), or None if nobody has counted yet."""
    record = snap.vtools.get(e['id'])
    if isinstance(record, dict) and record.get('ieee-attending') is not None:
        return record['ieee-attending'], record.get('guests-attending') or 0, "vTools"
    counts = snap.checkins.get(e['id'], {})
    if 'total' in counts:
        return counts['ieee'], counts['guests'], "check-in"
    return None


async def gather_month(year, month, now=None):
    """A Snapshot of the month's finished events, with attendance, check-ins and slides."""
    now = now or datetime.datetime.now(TZ)
    start, end = month_span(year, month)
    events = await asyncio.to_thread(fetch_calendar, between=(start, min(end, now)))
    events = [e for e in events if e['end'] <= now]
    store = EventStore().all()
    snap = Snapshot(events=events, store=store, fetched_at=now)
    async with aiohttp.ClientSession() as session:
        jobs = {"vTools": _vtools(session, events, store, snap), "GitHub": _slides(session, events, store, snap),
                "check-in forms": _checkins(events, store, snap)}
        results = await asyncio.gather(*jobs.values(), return_exceptions=True)
    for name, result in zip(jobs, results):
        if isinstance(result, Exception):
            snap.problems.append(f"{name}: {result}")
    return snap


def summarize(snap, year, month, guild=None, now=None):
    """Plain numbers for one month (JSON-safe, so they can be kept for comparison)."""
    now = now or datetime.datetime.now(TZ)
    start, end = month_span(year, month)
    rows, kinds, heard, follow = [], {}, Counter(), {key: [0, 0] for key, _ in FOLLOW_UP}
    for e in sorted(snap.events, key=lambda e: e['start']):
        if not start <= e['start'] < end:
            continue
        counted = attendance(e, snap)
        kind = KIND_NAMES[event_text.kind(e)]
        rows.append({"date": e['start'].date().isoformat(), "name": e['name'], "kind": kind,
                     "location": e['location'], "ieee": counted[0] if counted else None,
                     "guests": counted[1] if counted else None, "source": counted[2] if counted else ""})
        k = kinds.setdefault(kind, {"events": 0, "attendance": 0})
        k["events"] += 1
        k["attendance"] += sum(counted[:2]) if counted else 0
        heard.update(snap.checkins.get(e['id'], {}).get('heard') or {})
        for step in event_steps(e, snap, now):
            if step.key in follow and step.state != SKIP:
                follow[step.key][1] += 1
                follow[step.key][0] += step.state == DONE

    counted_rows = [r for r in rows if r["ieee"] is not None]
    ieee = sum(r["ieee"] for r in counted_rows)
    guests = sum(r["guests"] for r in counted_rows)
    summary = {
        "month": month_key(year, month), "complete": now >= end,
        "events": len(rows), "counted": len(counted_rows),
        "attendance": ieee + guests, "ieee": ieee, "guests": guests,
        "average": round((ieee + guests) / len(counted_rows), 1) if counted_rows else None,
        "kinds": kinds, "rows": rows, "heard": dict(heard.most_common()),
        "follow_up": follow, "problems": list(snap.problems),
    }
    summary.update(membership_numbers(year, month))
    if guild is not None:
        summary.update(discord_numbers(guild, start, end))
    return summary

def membership_numbers(year, month):
    start, end = (d.date() for d in month_span(year, month))
    records = members.load().values()
    decided = lambda m: start <= datetime.date.fromisoformat(m.get("decided") or m["submitted"]) < end
    return {"verified_new": sum(1 for m in records if m["status"] == "verified" and m.get("source") != "role" and decided(m)),
            "verified_from_role": sum(1 for m in records if m["status"] == "verified" and m.get("source") == "role" and decided(m)),
            "verified_year": sum(1 for m in records if m["status"] == "verified" and m["year"] == year),
            "pending": sum(1 for m in records if m["status"] == "pending")}

def discord_numbers(guild, start, end):
    """Server size now, and people who joined in the month and are still here."""
    humans = [m for m in guild.members if not m.bot]
    return {"discord_members": len(humans),
            "discord_joins": sum(1 for m in humans if m.joined_at and start <= m.joined_at < end)}


# ---------- history ----------

def remember(summary):
    """Keep a finished month's numbers for next month's comparison."""
    if not summary["complete"]:
        return
    state = load_json(METRICS_FILE, {"months": {}})
    state["months"][summary["month"]] = {k: v for k, v in summary.items() if k != "rows"}
    save_json(METRICS_FILE, state)

def remembered(year, month):
    return load_json(METRICS_FILE, {"months": {}})["months"].get(month_key(year, month))


# ---------- the report ----------

def change(now, before, key):
    if before is None or before.get(key) is None or now.get(key) is None:
        return ""
    diff = now[key] - before[key]
    return f" ({'+' if diff >= 0 else ''}{diff:g} vs last month)"

def report_embeds(summary, previous=None):
    year, month = map(int, summary["month"].split("-"))
    title = month_label(year, month) + ("" if summary["complete"] else " so far")
    s, p = summary, previous
    overview = [
        f"**{s['events']}** events{change(s, p, 'events')}",
        f"**{s['attendance']}** attendees{change(s, p, 'attendance')}: {s['ieee']} IEEE members, {s['guests']} guests",
    ]
    if s["average"] is not None:
        overview.append(f"**{s['average']:g}** per event on average{change(s, p, 'average')}")
    if s["counted"] < s["events"]:
        overview.append(f"⚠️ {s['events'] - s['counted']} event(s) have no count yet (no vTools report or check-in form)")
    if "discord_members" in s:
        overview.append(f"**{s['discord_joins']}** joined the Discord{change(s, p, 'discord_joins')} · "
                        f"{s['discord_members']} members now")
    overview.append(f"**{s['verified_new']}** IEEE memberships verified this month · {s['verified_year']} verified for {year}"
                    + (f" · {s['pending']} waiting" if s['pending'] else ""))
    embeds = [build_embed(f"📊 {title}", overview, IEEE_GOLD)]

    lines = []
    for r in s["rows"]:
        count = f"{r['ieee'] + r['guests']} ({r['ieee']} IEEE · {r['source']})" if r["ieee"] is not None else "not counted"
        lines.append(f"`{datetime.date.fromisoformat(r['date']):%b %-d}` **{r['name']}**: {count}")
    if s["kinds"]:
        lines.append("")
        lines += [f"{kind}: {k['events']} event(s), {k['attendance']} attendees" for kind, k in sorted(s["kinds"].items())]
    embeds.append(build_embed("🗓️ Events", lines or ["No events this month."], IEEE_BLUE))

    follow = [f"{label}: **{done}/{total}**" for (key, label) in FOLLOW_UP
              for done, total in [s["follow_up"][key]] if total]
    heard = [f"{source}: {n}" for source, n in s["heard"].items()]
    extra = (follow or ["Nothing to wrap up."]) + ([""] + ["**How people heard about us** (check-in forms)"] + heard if heard else [])
    if s["problems"]:
        extra += ["", "Couldn't read: " + "; ".join(s["problems"])]
    embeds.append(build_embed("✅ Follow-through", extra, IEEE_BLUE))
    return embeds

def csv_file(summary):
    """The month's events as CSV, for the annual report and vTools catch-up."""
    buffer = io.StringIO()
    writer = csv.writer(buffer)
    writer.writerow(["Date", "Event", "Kind", "Location", "IEEE members", "Guests", "Total", "Counted from"])
    for r in summary["rows"]:
        total = r["ieee"] + r["guests"] if r["ieee"] is not None else ""
        writer.writerow([r["date"], r["name"], r["kind"], r["location"],
                         "" if r["ieee"] is None else r["ieee"], "" if r["guests"] is None else r["guests"], total, r["source"]])
    return io.BytesIO(buffer.getvalue().encode()), f"ieee-cs-events-{summary['month']}.csv"
