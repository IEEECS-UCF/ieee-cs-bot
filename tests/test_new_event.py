import datetime
import types

import pytest

from bot.officer import new_event, views
from bot.officer.data import EventStore
from tests.officer_fixtures import NOW, event, snapshot

TZ = NOW.tzinfo


@pytest.mark.parametrize("date,time,start,end", [
    ("11/20", "6:30 PM - 8:30 PM", "2026-11-20 18:30", "20:30"),
    ("Nov 20", "6:30-8:30pm", "2026-11-20 18:30", "20:30"),
    ("2026-11-20", "18:30", "2026-11-20 18:30", "20:30"),
    ("11/20", "6", "2026-11-20 18:00", "20:00"),            # a bare hour is evening, not the 6th
    ("11/20", "11-1pm", "2026-11-20 11:00", "13:00"),
    ("11/20", "10am-2pm", "2026-11-20 10:00", "14:00"),
    ("11/20", "6:30 p.m. to 8:30 p.m.", "2026-11-20 18:30", "20:30"),
    ("1/15", "7 to 9", "2027-01-15 19:00", "21:00"),         # no year and already past: next January
])
def test_form_dates_and_times(date, time, start, end):
    s, e = new_event.parse_form(date, time, NOW)
    assert (s.strftime("%Y-%m-%d %H:%M"), e.strftime("%H:%M")) == (start, end)
    assert s.tzinfo == TZ

@pytest.mark.parametrize("date,time,message", [
    ("", "6", "Give a date"), ("blah", "6", "Couldn't read the date"),
    ("11/20", "", "Give a start time"), ("11/20", "9pm-8pm", "end after it starts"),
])
def test_form_errors_say_what_to_fix(date, time, message):
    with pytest.raises(ValueError, match=message):
        new_event.parse_form(date, time, NOW)

def test_calendar_body_leaves_out_unconfirmed_rooms():
    s, e = new_event.parse_form("11/20", "6-8", NOW)
    body = new_event.calendar_body("  Intro  to gem5 ", s, e, " Learn gem5. ", "")
    assert body["summary"] == "Intro to gem5" and "location" not in body
    assert body["start"]["timeZone"] == "America/New_York"
    assert new_event.calendar_body("x", s, e, "", "TCH 151")["location"] == "TCH 151"

def test_overlaps_flag_same_room_and_same_time():
    a = event(gid="a", location="ENG2 205", days=5)
    b = event(gid="b", location="TCH 151", days=5)
    c = event(gid="c", location="ENG2 205", days=6)
    found = new_event.overlaps([a, b, c], a["start"], a["end"], "Engineering II 205", lambda e: e["location"])
    assert [(e["id"], same) for e, same in found] == [("a", True), ("b", False)]


# ---------- preview and create ----------

class FakeCache:
    def __init__(self, snap):
        self.snapshot, self.fresh = snap, 0
    async def get(self, fresh=False):
        self.fresh += fresh
        return self.snapshot
    def with_store(self):
        return self.snapshot

class FakeResponse:
    def __init__(self):
        self.edited = self.sent = None
    def is_done(self):
        return self.edited is not None
    async def edit_message(self, **kwargs):
        self.edited = kwargs
    async def send_message(self, text, **kwargs):
        self.sent = text

def fake_interaction(sync):
    calls = {}
    async def edit_original_response(**kwargs):
        calls["content"] = kwargs["content"]
    return types.SimpleNamespace(
        user=types.SimpleNamespace(id=5), response=FakeResponse(), edit_original_response=edit_original_response,
        client=types.SimpleNamespace(get_cog=lambda name: sync)), calls

def values(**change):
    v = {"name": "Intro to gem5", "date": "11/12", "time": "6:30-8:30 PM", "room": "ENG2 205", "description": "Sim."}
    v.update(change)
    return v

async def test_preview_warns_about_clashes():
    existing = event(gid="x", name="Vulkan Workshop", location="ENG2 205", days=10)
    cog = types.SimpleNamespace(cache=FakeCache(snapshot([existing])))
    start = existing["start"]
    preview = views.NewEventPreview(cog, values(), start, start + datetime.timedelta(hours=2))
    text = (await preview.embed()).description
    assert "Same room as **Vulkan Workshop**" in text
    assert {b.label for b in preview.children if b.row is None} == {"Create · room confirmed", "Create · room only requested"}

async def test_create_requested_room_stays_off_the_calendar(monkeypatch):
    inserted, opened = [], []
    monkeypatch.setattr(views, "insert_event", lambda cal, body: inserted.append(body) or {"id": "newgid"})
    async def fake_open(cog, interaction, gid):
        opened.append(gid)
    monkeypatch.setattr(views, "open_card", fake_open)
    sync = types.SimpleNamespace(sync_soon=lambda delay: setattr(sync, "soon", delay))
    cog = types.SimpleNamespace(cache=FakeCache(snapshot([])))
    start = NOW + datetime.timedelta(days=10)
    preview = views.NewEventPreview(cog, values(), start, start + datetime.timedelta(hours=2))
    interaction, calls = fake_interaction(sync)
    await preview.create(interaction, "requested")
    assert "location" not in inserted[0] and inserted[0]["summary"] == "Intro to gem5"
    assert EventStore().get("newgid")["room"]["status"] == "requested"
    assert opened == ["newgid"] and sync.soon == 5 and cog.cache.fresh == 1
    again, _ = fake_interaction(sync)
    await preview.create(again, "requested")
    assert again.response.sent == "This was already created or cancelled." and len(inserted) == 1

async def test_create_confirmed_room_goes_on_the_calendar(monkeypatch):
    inserted = []
    monkeypatch.setattr(views, "insert_event", lambda cal, body: inserted.append(body) or {"id": "g2"})
    async def fake_open(cog, interaction, gid):
        pass
    monkeypatch.setattr(views, "open_card", fake_open)
    cog = types.SimpleNamespace(cache=FakeCache(snapshot([])))
    start = NOW + datetime.timedelta(days=10)
    preview = views.NewEventPreview(cog, values(), start, start + datetime.timedelta(hours=2))
    interaction, calls = fake_interaction(None)
    await preview.create(interaction, "confirmed")
    assert inserted[0]["location"] == "ENG2 205" and EventStore().get("g2")["room"]["status"] == "confirmed"
    assert calls["content"].startswith("Created **Intro to gem5**")
