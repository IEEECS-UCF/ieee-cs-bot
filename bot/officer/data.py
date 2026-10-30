import os
import json
import datetime

# Everything officers set about an event lives in one file, keyed by the
# Google Calendar event ID (the calendar is the source of truth for events):
#
#   {"<calendar id>": {
#       "owner": <user id>,
#       "room": {"status": "requested|confirmed|denied", "room": "...", "since": "YYYY-MM-DD", "by": <id>, "note": ""},
#       "links": {"knightconnect": {"id", "link"} | "not needed", "vtools": {...} | "not needed"},
#       "checkin": {"formId", "url", "shortUrl", "linked": bool},
#       "slides": {"name", "url"},
#       "marks": {"<step>": "done" | "not needed"},
#       "reminder": {"send": "auto|always|never", "ping": "auto|none|everyone|workshops|socials|prodev"},
#   }}
EVENTS_FILE = "data/events.json"
DEADLINES_FILE = "bot/deadlines.json"
DEADLINES_EXTRA_FILE = "data/deadlines_extra.json"  # deadlines added or hidden from Discord
NUDGES_FILE = "data/nudges.json"                    # {"sent": {"<event>|<step>": date}, "threads": {...}, "day": date}
DIGEST_STATE_FILE = "data/digest.json"

# Files the event store replaced; read once by migrate() and then renamed
OLD_FILES = {
    "owners": "data/owners.json", "rooms": "data/rooms.json", "wrapups": "data/wrapups.json",
    "checkins": "data/checkins.json", "reminders": "data/reminder_overrides.json",
}
SKIPPED = "not needed"


def load_json(path, default):
    """The file's data, or default if it's missing. A corrupt file is moved
    aside (never overwritten) so the bot keeps working and nothing is lost."""
    if not os.path.exists(path):
        return default
    try:
        with open(path) as f:
            return json.load(f)
    except (json.JSONDecodeError, UnicodeDecodeError) as e:
        aside = f"{path}.corrupt-{datetime.datetime.now():%Y%m%d-%H%M%S}"
        os.replace(path, aside)
        print(f"ERROR: {path} was unreadable ({e}); moved it to {aside} and started fresh.")
        return default

def save_json(path, data):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    tmp = path + ".tmp"
    with open(tmp, "w") as f:
        json.dump(data, f, indent=2)
    os.replace(tmp, path)


class EventStore:
    """Officer-set facts about events. Reads the file each time, so every part
    of the bot (and every restart) sees the same thing."""

    def all(self):
        return load_json(EVENTS_FILE, {})

    def get(self, gid):
        return self.all().get(gid, {})

    def update(self, gid, change):
        """Apply change(record) to one event's record and save it."""
        data = self.all()
        record = data.setdefault(gid, {})
        change(record)
        if record:
            data[gid] = record
        else:
            data.pop(gid, None)
        save_json(EVENTS_FILE, data)
        return record

    # Small helpers so callers don't repeat the dict plumbing

    def set_owner(self, gid, user_id):
        def change(r):
            if user_id:
                r["owner"] = user_id
            else:
                r.pop("owner", None)
        return self.update(gid, change)

    def set_room(self, gid, status, room, by, note=""):
        def change(r):
            if status == "clear":
                r.pop("room", None)
                return
            previous = r.get("room", {})
            same = previous.get("status") == status and previous.get("room") == room
            r["room"] = {"status": status, "room": room, "by": by, "note": note or previous.get("note", ""),
                         "since": previous["since"] if same else datetime.date.today().isoformat()}
        return self.update(gid, change)

    def set_link(self, platform, gid, value):
        """value: {"id", "link"}, SKIPPED, or None to match automatically again."""
        def change(r):
            links = r.setdefault("links", {})
            if value is None:
                links.pop(platform, None)
            else:
                links[platform] = value
            if not links:
                r.pop("links", None)
        return self.update(gid, change)

    def mark(self, gid, step, value):
        """value: "done", SKIPPED, or None to clear."""
        def change(r):
            marks = r.setdefault("marks", {})
            if value is None:
                marks.pop(step, None)
            else:
                marks[step] = value
            if not marks:
                r.pop("marks", None)
        return self.update(gid, change)

    def set_slides(self, gid, value):
        def change(r):
            if value:
                r["slides"] = value
            else:
                r.pop("slides", None)
        return self.update(gid, change)

    def set_checkin(self, gid, form):
        return self.update(gid, lambda r: r.__setitem__("checkin", form))

    def set_reminder(self, gid, send=None, ping=None):
        def change(r):
            reminder = r.setdefault("reminder", {})
            if send:
                reminder["send"] = send
            if ping:
                reminder["ping"] = ping
            for key in [k for k, v in reminder.items() if v == "auto"]:
                del reminder[key]
            if not reminder:
                r.pop("reminder", None)
        return self.update(gid, change)


def migrate(discord_to_calendar):
    """Fold the old per-feature files into events.json (once).

    discord_to_calendar maps Discord event IDs to calendar IDs, for the old
    reminder settings, which were keyed by Discord event.
    """
    present = {name: path for name, path in OLD_FILES.items() if os.path.exists(path)}
    if not present:
        return 0
    data = load_json(EVENTS_FILE, {})
    rec = lambda gid: data.setdefault(gid, {})
    for gid, user in load_json(OLD_FILES["owners"], {}).items():
        rec(gid)["owner"] = user
    for gid, room in load_json(OLD_FILES["rooms"], {}).items():
        rec(gid)["room"] = room
    for gid, form in load_json(OLD_FILES["checkins"], {}).items():
        rec(gid)["checkin"] = {k: form[k] for k in ("formId", "url", "shortUrl") if k in form} | {"linked": form.get("linked", False)}
    for gid, marks in load_json(OLD_FILES["wrapups"], {}).items():
        r = rec(gid)
        for platform in ("vtools", "knightconnect"):
            if platform in marks:
                r.setdefault("links", {})[platform] = marks[platform]
        if "slides_link" in marks:
            r["slides"] = marks["slides_link"]
        for step in ("slides", "recap"):
            if step in marks:
                r.setdefault("marks", {})[step] = marks[step]
    for did, override in load_json(OLD_FILES["reminders"], {}).items():
        gid = discord_to_calendar.get(int(did)) or discord_to_calendar.get(did)
        if gid:
            rec(gid)["reminder"] = {k: v for k, v in override.items() if k in ("send", "ping") and v != "auto"}
    save_json(EVENTS_FILE, {gid: r for gid, r in data.items() if r})
    for path in present.values():
        os.replace(path, path + ".migrated")
    return len(present)


def deadline_key(day, what):
    return f"{day.isoformat()}|{what}"

def load_deadlines():
    """The shipped list plus deadlines officers added, minus ones they hid."""
    extra = load_json(DEADLINES_EXTRA_FILE, {"added": [], "hidden": []})
    entries = load_json(DEADLINES_FILE, []) + extra["added"]
    hidden = set(extra["hidden"])
    deadlines = [(datetime.date.fromisoformat(d['date']), d['what']) for d in entries]
    return sorted(d for d in deadlines if deadline_key(*d) not in hidden)

def add_deadline(day, what):
    extra = load_json(DEADLINES_EXTRA_FILE, {"added": [], "hidden": []})
    key = deadline_key(day, what)
    extra["hidden"] = [k for k in extra["hidden"] if k != key]
    if not any(deadline_key(datetime.date.fromisoformat(d['date']), d['what']) == key for d in extra["added"]):
        extra["added"].append({"date": day.isoformat(), "what": what})
    save_json(DEADLINES_EXTRA_FILE, extra)

def remove_deadline(key):
    """Delete an added deadline, or hide a shipped one."""
    extra = load_json(DEADLINES_EXTRA_FILE, {"added": [], "hidden": []})
    before = len(extra["added"])
    extra["added"] = [d for d in extra["added"]
                      if deadline_key(datetime.date.fromisoformat(d['date']), d['what']) != key]
    if len(extra["added"]) == before:
        extra["hidden"].append(key)
    save_json(DEADLINES_EXTRA_FILE, extra)
