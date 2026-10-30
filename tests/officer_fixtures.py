import datetime

from bot.officer.sources import Snapshot, TZ

NOW = datetime.datetime(2026, 11, 2, 12, 0, tzinfo=TZ)   # a Monday at noon, fixed so tests don't drift


def event(name="Bash Scripting Workshop", location="ENG2 205", days=10, hours=2, description="Learn bash.",
          gid="g1", flyer="https://example.com/flyer.png"):
    start = NOW.replace(hour=18, minute=30) + datetime.timedelta(days=days)
    return {"id": gid, "name": name, "location": location, "start": start,
            "end": start + datetime.timedelta(hours=hours), "description": description, "flyer": flyer}

def snapshot(events, store=None, **kwargs):
    snap = Snapshot(events=events, store=store or {}, fetched_at=NOW, **kwargs)
    for e in events:
        snap.knightconnect.setdefault(e['id'], None)
        snap.vtools.setdefault(e['id'], None)
    return snap

def listing(e, platform, **change):
    """A KnightConnect or vTools record that matches e exactly, with changes."""
    if platform == "knightconnect":
        record = {"id": 9, "name": e["name"], "startsOn": e["start"].isoformat(), "location": e["location"], "link": "https://kc/9"}
    else:
        record = {"id": 7, "title": e["name"], "start-time": e["start"].isoformat(), "building": "",
                  "room-number": e["location"], "address1": "", "link": "https://vt/7", "ieee-attending": None}
    record.update(change)
    return record
