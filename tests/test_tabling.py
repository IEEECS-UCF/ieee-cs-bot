import datetime
import re
import types

import pytest

from bot.cogs import volunteer
from bot.cogs import officer as officer_cog
from bot.officer import tabling, sources
from tests.officer_fixtures import NOW

TZ = NOW.tzinfo


def member(i, username, global_name=None, nick=None):
    return types.SimpleNamespace(id=i, bot=False, name=username, global_name=global_name, nick=nick,
                                 display_name=nick or global_name or username)

MEMBERS = [member(1, "harrisonh", "Harrison"), member(2, "jvertus", "Jack 🪻"),
           member(3, "harrison99", "Harrison Park"), member(4, "alexander"), member(5, "dawn_s", "Dawn")]

def rows():
    return [{"Date": "2026-11-06", "Time": "3:00 PM - 5:00 PM", "Event Name": "Transfer Resource Fair",
             "Location": "SU Room 316", "Volunteer 1": "Elionel Vertus", "Volunteer 2": "", "Notes": ""},
            {"Date": "2026-11-13", "Time": "3:00 PM - 5:00 PM", "Event Name": "Transfer Resource Fair",
             "Location": "SU Room 316", "Volunteer 1": "", "Volunteer 2": "", "Notes": "Bring the banner"},
            {"Date": "not a date", "Event Name": "x"}]


def test_names_match_only_when_sure():
    assert tabling.match_person("Harrison H.", MEMBERS).id == 1        # first name + initial
    assert tabling.match_person("Harrison P", MEMBERS).id == 3
    assert tabling.match_person("Dawn", MEMBERS).id == 5                # the only Dawn
    assert tabling.match_person("Alex", MEMBERS) is None                # "alexander" isn't "Alex"
    assert tabling.match_person("Elionel Vertus", MEMBERS) is None      # Discord calls him Jack

def test_officer_choice_wins():
    tabling.set_person("Elionel Vertus", 2)
    assert tabling.resolve("elionel  vertus", MEMBERS) == (2, True)
    tabling.set_person("Dawn", tabling.NOT_IN_DISCORD)
    assert tabling.resolve("Dawn", MEMBERS) == (None, True)
    tabling.set_person("Dawn", None)
    assert tabling.resolve("Dawn", MEMBERS) == (5, False)

def test_sessions_and_what_is_due():
    sessions = tabling.sessions(rows())
    assert [s["key"] for s in sessions] == ["2026-11-06|3:00 PM - 5:00 PM", "2026-11-13|3:00 PM - 5:00 PM"]
    nov5 = datetime.datetime(2026, 11, 5, 12, 30, tzinfo=TZ)
    assert [s["date"].day for s in tabling.due(sessions, {}, nov5, 12)] == [6]
    assert tabling.due(sessions, {}, nov5.replace(hour=11), 12) == []
    assert tabling.due(sessions, {sessions[0]["key"]: "2026-11-05"}, nov5, 12) == []

def test_reminder_pings_signups_and_board_claims():
    tabling.set_person("Elionel Vertus", 2)
    first, second = tabling.sessions(rows())
    text, pings = tabling.reminder(first, MEMBERS, claimed=[5], board_link="https://discord.com/channels/1/2/3")
    assert pings == [2, 5] and "Signed up: <@2>, <@5>" in text and "still open" not in text
    text, pings = tabling.reminder(second, MEMBERS, claimed=[])
    assert pings == [] and "Nobody is signed up" in text and "2 spots still open" in text and "Bring the banner" in text

def test_unmatched_names_are_shown_without_a_ping():
    session = tabling.sessions([dict(rows()[0], **{"Volunteer 1": "Somebody New"})])[0]
    text, pings = tabling.reminder(session, MEMBERS, claimed=[])
    assert pings == [] and "Signed up: Somebody New" in text and "1 spot still open" in text

def test_sent_reminders_are_forgotten_after_a_month():
    tabling.mark_sent("old", datetime.date(2026, 9, 1))
    tabling.mark_sent("new", datetime.date(2026, 11, 5))
    assert set(tabling.load_state()["sent"]) == {"new"}


# ---------- the loop ----------

class Channel:
    def __init__(self):
        self.sent = []
    async def send(self, text, allowed_mentions=None):
        self.sent.append((text, [u.id for u in allowed_mentions.users]))

async def test_loop_posts_once_with_board_claims(monkeypatch):
    tabling.set_person("Elionel Vertus", 2)
    task = volunteer.new_task("Table", "", "", 1, 9, key="tabling|2026-11-06|3:00 PM - 5:00 PM")
    task.update(claimed=[5], channel_id=7, message_id=8)
    volunteer.save_tasks({task["id"]: task})
    channel = Channel()
    cog = officer_cog.Officer.__new__(officer_cog.Officer)
    cog.tabling_channel_id = 42
    cog.bot = types.SimpleNamespace(get_channel=lambda cid: channel,
                                    get_guild=lambda gid: types.SimpleNamespace(members=MEMBERS))
    async def fake_fetch(session):
        return rows()
    monkeypatch.setattr(officer_cog, "fetch_tabling", fake_fetch)
    class Clock(datetime.datetime):
        @classmethod
        def now(cls, tz=None):
            return datetime.datetime(2026, 11, 5, 12, 30, tzinfo=TZ)
    monkeypatch.setattr(officer_cog.datetime, "datetime", Clock)
    await cog.send_tabling_reminders_if_due()
    await cog.send_tabling_reminders_if_due()
    assert len(channel.sent) == 1
    text, pinged = channel.sent[0]
    assert pinged == [2, 5] and "Transfer Resource Fair" in text


# ---------- keeping an eye on the sheet ----------

def test_dates_need_a_year_and_bad_ones_are_reported():
    assert tabling.parse_day("2026-11-06") == datetime.date(2026, 11, 6)
    assert tabling.parse_day("11/6/2026") == datetime.date(2026, 11, 6)
    assert tabling.parse_day("Nov 6, 2026") == datetime.date(2026, 11, 6)
    assert tabling.parse_day("Nov 6") is None and tabling.parse_day("") is None
    bad = rows()[:2] + [dict(rows()[0], Date="Friday")]
    assert len(tabling.sessions(bad)) == 2
    assert '1 session(s) have a date the bot can\'t read ("Friday")' in tabling.sheet_problem(bad)
    assert tabling.sheet_problem(rows()[:2]) is None

class FakeResponse:
    def __init__(self, status, kind, text):
        self.status, self.headers, self._text = status, {"Content-Type": kind}, text
    async def text(self):
        return self._text
    async def __aenter__(self):
        return self
    async def __aexit__(self, *exc):
        return False

class FakeSession:
    def __init__(self, response):
        self.response = response
    def get(self, url, **kwargs):
        return self.response

@pytest.mark.parametrize("status,kind,text,message", [
    (404, "text/html", "", "HTTP 404"),
    (200, "text/html; charset=utf-8", "<html>Sign in</html>", "sign-in page"),
    (200, "text/csv", "Name,When\nx,y\n", "no \"Event Name\" column"),
    (200, "text/csv", ",Event Name,Date,Time\n,x,2026-11-06,3\n", "Column(s) missing: Location, Volunteer 1, Volunteer 2"),
])
async def test_sheet_errors_say_what_to_fix(status, kind, text, message):
    with pytest.raises(sources.TablingSheetError, match=re.escape(message)):
        await sources.fetch_tabling(FakeSession(FakeResponse(status, kind, text)))

async def test_sheet_reads_normally():
    text = ",Event Name,Date,Time,Location,Volunteer 1,Volunteer 2,Notes\n,Fair,2026-11-06,3 PM,SU,Jack,,\n,,,,,,,\n"
    got = await sources.fetch_tabling(FakeSession(FakeResponse(200, "text/csv", text)))
    assert len(got) == 1 and got[0]["Volunteer 1"] == "Jack"

async def test_broken_sheet_alerts_once_a_day_after_a_few_misses(monkeypatch):
    alerts = []
    async def fake_alert(bot, where, error=None, detail=None):
        alerts.append(detail)
    async def broken(session):
        raise sources.TablingSheetError("Google returned HTTP 404.")
    monkeypatch.setattr(officer_cog, "alert", fake_alert)
    monkeypatch.setattr(officer_cog, "fetch_tabling", broken)
    cog = officer_cog.Officer.__new__(officer_cog.Officer)
    cog.bot = None
    for _ in range(officer_cog.TABLING_ALERT_AFTER - 1):
        assert await cog.check_tabling_sheet() is None
    assert alerts == []
    await cog.check_tabling_sheet()
    await cog.check_tabling_sheet()
    assert len(alerts) == 1 and alerts[0].startswith("Google returned HTTP 404.")
    async def fixed(session):
        return rows()[:2]
    monkeypatch.setattr(officer_cog, "fetch_tabling", fixed)
    assert len(await cog.check_tabling_sheet()) == 2
    assert "problem" not in tabling.load_state() and "failures" not in tabling.load_state()

def test_chapter_view_shows_why_the_sheet_failed():
    from bot.officer import render
    embeds = render.chapter_embeds(None, {}, datetime.date(2026, 11, 2), tabling_error="Google returned HTTP 404.")
    assert "Couldn't read the tabling sheet: Google returned HTTP 404." in embeds[1].description
