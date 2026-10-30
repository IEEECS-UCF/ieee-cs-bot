import re
import datetime

from dateutil import parser

from bot.officer.sources import TZ

# Parsing for the New event form on /officer events. The form creates the
# Google Calendar event; calendar sync then makes the Discord event once it's
# within LOOKAHEAD_DAYS, so the calendar stays the one place events start.

DEFAULT_HOURS = 2
TIME_SPLIT = re.compile(r"\s*(?:-|–|—|\bto\b|\buntil\b)\s*", re.I)
MERIDIEM = re.compile(r"\d\s*([ap])\.?m?\.?\s*$", re.I)


def parse_date(text, today):
    """'11/20', 'Nov 20', '2026-11-20'. Without a year, the next such date."""
    text = (text or "").strip()
    if not text:
        raise ValueError("Give a date, like 11/20 or Nov 20.")
    try:
        day = parser.parse(text, default=datetime.datetime(today.year, 1, 1)).date()
    except (ValueError, OverflowError):
        raise ValueError(f"Couldn't read the date \"{text}\". Try 11/20 or Nov 20.")
    has_year = bool(re.search(r"\d{4}", text))
    if not has_year and day < today:
        day = day.replace(year=day.year + 1)
    return day

def parse_clock(text, day, meridiem=None):
    """One clock time on day. With no am/pm, 1-11 o'clock means afternoon or evening."""
    text = re.sub(r"^(\d{1,2})(?=\s*(?:[ap]\.?m?\.?)?\s*$)", r"\1:00", text.strip(), flags=re.I)  # "6" is a time, not a day
    given = MERIDIEM.search(text)
    if meridiem and not given:
        text, given = f"{text} {meridiem}m", True
    moment = parser.parse(text, default=datetime.datetime.combine(day, datetime.time())).replace(tzinfo=TZ)
    if not given and 1 <= moment.hour <= 11:
        moment += datetime.timedelta(hours=12)
    return moment

def parse_times(text, day):
    """'6:30 PM - 8:30 PM', '6:30-8:30pm', '18:30'. Without an end, DEFAULT_HOURS long."""
    text = (text or "").strip()
    if not text:
        raise ValueError("Give a start time, like 6:30 PM or 6:30-8:30 PM.")
    parts = [p for p in TIME_SPLIT.split(text, maxsplit=1) if p]
    try:
        end_meridiem = MERIDIEM.search(parts[1]).group(1).lower() if len(parts) > 1 and MERIDIEM.search(parts[1]) else None
        start = parse_clock(parts[0], day, end_meridiem)
        end = parse_clock(parts[1], day) if len(parts) > 1 else start + datetime.timedelta(hours=DEFAULT_HOURS)
        if end_meridiem and start >= end and not MERIDIEM.search(parts[0]):
            start -= datetime.timedelta(hours=12)   # "11-1pm" starts in the morning
    except (ValueError, OverflowError):
        raise ValueError(f"Couldn't read the time \"{text}\". Try 6:30 PM or 6:30-8:30 PM.")
    if end <= start:
        raise ValueError("The event has to end after it starts.")
    return start, end

def parse_form(date_text, time_text, now):
    return parse_times(time_text, parse_date(date_text, now.date()))

def calendar_body(title, start, end, description, location):
    body = {"summary": " ".join(title.split()), "description": description.strip(),
            "start": {"dateTime": start.isoformat(), "timeZone": str(TZ)},
            "end": {"dateTime": end.isoformat(), "timeZone": str(TZ)}}
    if location:
        body["location"] = location
    return body
