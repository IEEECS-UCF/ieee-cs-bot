import re
import datetime

from bot.officer.data import load_json, save_json

# Day-before reminders for tabling sessions on the Tabling Signup sheet. The
# sheet has names, not Discord accounts, so names are matched to server
# members automatically; officers fix any wrong or missing match from
# /officer chapter, and that choice always wins.
#
#   data/people.json      {"<name, lowercased>": <user id> | "none"}
#   data/tabling.json     {"sent": {"<date>|<time>": "<date sent>"}}
PEOPLE_FILE = "data/people.json"
STATE_FILE = "data/tabling.json"
NOT_IN_DISCORD = "none"
SLOTS = ('Volunteer 1', 'Volunteer 2')


def tokens(text):
    return re.sub(r"[^a-z0-9 ]", " ", (text or "").casefold()).split()

def member_names(member):
    return {n for n in (getattr(member, 'nick', None), getattr(member, 'global_name', None),
                        getattr(member, 'display_name', None), getattr(member, 'name', None)) if n}

def fit(sheet_name, member):
    """3 same name, 2 first name plus surname or initial, 1 first name only, 0 no match."""
    want = tokens(sheet_name)
    if not want:
        return 0
    first, rest = want[0], want[1:]
    best = 0
    for name in member_names(member):
        have = tokens(name)
        if have == want:
            return 3
        if have and have[0] == first:
            others = have[1:]
        elif len(have) == 1 and have[0].startswith(first) and rest:
            others = [have[0][len(first):]]   # a username like "harrisonh"
        else:
            continue
        if rest and all(any(h.startswith(r) for h in others) for r in rest):
            best = max(best, 2)
        elif have[0] == first:
            best = max(best, 1)
    return best

def match_person(sheet_name, members):
    """The one member this sheet name most likely is, or None if unsure.

    A first name alone only counts when exactly one person in the server has it.
    """
    scored = [(fit(sheet_name, m), m) for m in members if not getattr(m, 'bot', False)]
    top = max((s for s, _ in scored), default=0)
    if top == 0:
        return None
    best = [m for s, m in scored if s == top]
    return best[0] if len(best) == 1 else None


# ---------- officer overrides ----------

def load_people():
    return load_json(PEOPLE_FILE, {})

def set_person(name, user_id):
    """user_id: a member's ID, NOT_IN_DISCORD, or None to go back to matching automatically."""
    people = load_people()
    key = " ".join(tokens(name))
    if user_id is None:
        people.pop(key, None)
    else:
        people[key] = user_id
    save_json(PEOPLE_FILE, people)

def resolve(name, members, people=None):
    """(user id or None, set by hand?) for a name on the sheet."""
    people = load_people() if people is None else people
    chosen = people.get(" ".join(tokens(name)))
    if chosen == NOT_IN_DISCORD:
        return None, True
    if chosen:
        return int(chosen), True
    member = match_person(name, members)
    return (member.id if member else None), False


# ---------- sessions and reminders ----------

def sessions(rows):
    """Sheet rows as sessions: {"date", "time", "event", "location", "names", "notes", "key"}."""
    found = []
    for r in rows or []:
        try:
            day = datetime.date.fromisoformat(r.get('Date', '').strip())
        except ValueError:
            continue
        time = r.get('Time', '').strip()
        found.append({"date": day, "time": time, "event": r.get('Event Name', '').strip(),
                      "location": r.get('Location', '').strip(), "notes": r.get('Notes', '').strip(),
                      "names": [r.get(k, '').strip() for k in SLOTS if r.get(k, '').strip()],
                      "key": f"{day.isoformat()}|{time}"})
    return found

def due(all_sessions, sent, now, hour):
    """Sessions tomorrow whose reminder hasn't gone out, once it's past hour today."""
    if now.hour < hour:
        return []
    tomorrow = now.date() + datetime.timedelta(days=1)
    return [s for s in all_sessions if s["date"] == tomorrow and s["key"] not in sent]

def reminder(session, members, claimed, board_link=None, people=None):
    """(text, user IDs to ping) for one session.

    claimed: user IDs who signed up on the volunteer board for this session.
    """
    pings, shown = [], []
    for name in session["names"]:
        user_id, _ = resolve(name, members, people)
        if user_id and user_id not in pings:
            pings.append(user_id)
            shown.append(f"<@{user_id}>")
        elif not user_id:
            shown.append(name)
    for user_id in claimed:
        if user_id not in pings:
            pings.append(user_id)
            shown.append(f"<@{user_id}>")
    open_spots = max(0, len(SLOTS) - len(shown))
    lines = [f"🪧 **Tabling tomorrow: {session['event']}**",
             f"🗓️ {session['date']:%a %b %-d} · {session['time']} · 📍 {session['location']}"]
    if shown:
        lines.append("Signed up: " + ", ".join(shown) + ". Thank you! 💙")
    else:
        lines.append("🔴 **Nobody is signed up yet.**")
    if open_spots:
        where = f"claim it on the volunteer board: {board_link}" if board_link else "add your name to the Tabling Signup sheet"
        lines.append(f"{open_spots} spot{'s' if open_spots > 1 else ''} still open; {where}")
    if session["notes"]:
        lines.append(f"Notes: {session['notes']}")
    return "\n".join(lines), pings

def load_state():
    return load_json(STATE_FILE, {"sent": {}})

def mark_sent(key, today):
    state = load_state()
    state["sent"][key] = today.isoformat()
    cutoff = (today - datetime.timedelta(days=30)).isoformat()
    state["sent"] = {k: v for k, v in state["sent"].items() if v >= cutoff}
    save_json(STATE_FILE, state)
