import datetime

from bot.officer.data import load_json, save_json

# IEEE membership checks. Members submit their number with /member verify; an
# officer looks it up in IEEE's validator (it needs an IEEE sign-in, and IEEE
# only offers an API to large conferences) and approves or rejects it.
#
#   {"<user id>": {"number": "09994411", "email": "", "status": "pending|verified|rejected",
#                  "year": 2026, "submitted": "YYYY-MM-DD", "reviewed_by": <id>, "reason": "",
#                  "card": {"channel": <id>, "message": <id>}, "source": "role"}}
# "source": "role" marks people recorded because they already had the Dues Paying role.
MEMBERS_FILE = "data/members.json"
VALIDATOR = "https://services20.ieee.org/membership-validator.html"


def normalize_number(text):
    """IEEE member numbers are 8 digits with leading zeros; None if it isn't one."""
    digits = "".join(ch for ch in (text or "") if ch.isdigit())
    if not digits or len(digits) > 8 or len(digits) != len((text or "").strip().replace(" ", "")):
        return None
    return digits.zfill(8)

def load():
    return load_json(MEMBERS_FILE, {})

def get(user_id):
    return load().get(str(user_id))

def save_record(user_id, record):
    members = load()
    members[str(user_id)] = record
    save_json(MEMBERS_FILE, members)

def submit(user_id, number, email, today=None):
    today = today or datetime.date.today()
    record = {"number": number, "email": email or "", "status": "pending", "year": today.year,
              "submitted": today.isoformat(), "reviewed_by": None, "reason": "", "card": None}
    save_record(user_id, record)
    return record

def decide(user_id, status, officer_id, reason="", today=None):
    record = get(user_id)
    if record is None:
        return None
    today = today or datetime.date.today()
    record.update(status=status, reviewed_by=officer_id, reason=reason or "", year=today.year,
                  decided=today.isoformat())
    save_record(user_id, record)
    return record

def recognize(user_id, today=None, renew=False):
    """Record someone who has the Dues Paying role as verified this year.

    Without renew, only people with no record yet are recorded: that covers
    the role given before /member verify existed. Memberships are yearly, so
    still holding the role in a new year doesn't renew anything; renew=True is
    for an officer adding the role by hand, which is a fresh decision.
    Returns the new record, or None if nothing changed.
    """
    today = today or datetime.date.today()
    record = get(user_id)
    if record and not renew:
        return None
    if record and record["status"] == "verified" and record["year"] == today.year:
        return None
    record = {"number": (record or {}).get("number"), "email": (record or {}).get("email", ""),
              "status": "verified", "source": "role", "year": today.year, "submitted": today.isoformat(),
              "decided": today.isoformat(), "reviewed_by": None, "reason": "", "card": (record or {}).get("card")}
    save_record(user_id, record)
    return record

def revoke(user_id, today=None):
    """The Dues Paying role was taken away: this year's verification no longer stands."""
    today = today or datetime.date.today()
    record = get(user_id)
    if not record or record["status"] != "verified" or record["year"] != today.year:
        return None
    record.update(status="rejected", reason="the Dues Paying role was removed", decided=today.isoformat())
    save_record(user_id, record)
    return record

def counts(today=None):
    """(verified this year, [(user id, record) waiting for review]) for the chapter view."""
    year = (today or datetime.date.today()).year
    members = load()
    return (sum(1 for m in members.values() if m["status"] == "verified" and m["year"] == year),
            [(int(uid), m) for uid, m in members.items() if m["status"] == "pending"])
