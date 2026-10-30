import copy
import datetime
import types

import discord
import pytest

from bot.cogs import calendar_sync as cs

TZ = "America/New_York"
BOT = 999
NOW = datetime.datetime.now(datetime.timezone.utc).replace(microsecond=0)


class Asset:
    def __init__(self, key):
        self.key, self.url = key, f"https://cdn.discordapp.com/guild-events/1/{key}.png"

class Event:
    next_id = 100
    def __init__(self, name, desc, loc, start, end, creator=BOT, cover=None):
        Event.next_id += 1
        self.id, self.name, self.description, self.location = Event.next_id, name, desc, loc
        self.start_time, self.end_time, self.creator_id, self.channel = start, end, creator, None
        self.status, self.cover_image = discord.EventStatus.scheduled, (Asset(cover) if cover else None)
    async def edit(self, **kw):
        CALLS.append(("discord.edit", self.name, sorted(kw)))
        for k, v in kw.items():
            if k == "image":
                self.cover_image = Asset(f"img{len(CALLS)}")
            else:
                setattr(self, k, v)
        return self
    async def delete(self, reason=None):
        CALLS.append(("discord.delete", self.name))

class Guild:
    def __init__(self, events):
        self.events, self.fail_lookups = events, False
    async def create_scheduled_event(self, **kw):
        e = Event(kw['name'], kw['description'], kw['location'], kw['start_time'], kw['end_time'])
        CALLS.append(("discord.create", e.name))
        return e
    async def fetch_scheduled_event(self, did):
        if self.fail_lookups:
            raise discord.HTTPException(types.SimpleNamespace(status=500, reason="Server Error"), "boom")
        for e in self.events:
            if e.id == did:
                return e
        raise discord.NotFound(types.SimpleNamespace(status=404, reason="Not Found"), "gone")


CALLS, ITEMS = [], {}

def item(gid, name, desc="About it", loc="ENG2 205", days=5, hours=2):
    start = NOW + datetime.timedelta(days=days)
    return {"id": gid, "summary": name, "description": desc, "location": loc,
            "start": {"dateTime": start.isoformat()},
            "end": {"dateTime": (start + datetime.timedelta(hours=hours)).isoformat()}}

@pytest.fixture(autouse=True)
def fake_calendar(monkeypatch):
    CALLS.clear()
    ITEMS.clear()
    def patch(cal, gid, body):
        CALLS.append(("gcal.patch", gid, dict(body)))
        updated = copy.deepcopy(ITEMS[gid])
        updated.update(body)
        ITEMS[gid] = updated
        return updated
    def insert(cal, body):
        gid = f"new{len(CALLS)}"
        CALLS.append(("gcal.insert", body['summary']))
        ITEMS[gid] = dict(body, id=gid)
        return ITEMS[gid]
    monkeypatch.setattr(cs, "patch_event", patch)
    monkeypatch.setattr(cs, "insert_event", insert)
    monkeypatch.setattr(cs, "delete_gcal_event", lambda cal, gid: CALLS.append(("gcal.delete", gid)))
    monkeypatch.setattr(cs, "get_event", lambda cal, gid: ITEMS.get(gid))

def new_cog(can_write):
    cog = object.__new__(cs.CalendarSync)
    cog.bot = types.SimpleNamespace(user=types.SimpleNamespace(id=BOT))
    cog.calendar_id, cog.can_write = "cal", can_write
    cog.state = {"event_map": {}, "snapshots": {}, "summary_message_id": None, "summary_week": None}
    async def flyer(url):
        CALLS.append(("download", url))
        return b"PNG" if url else None
    cog.download_flyer = flyer
    return cog

async def sync(cog, guild):
    payloads = [(gid, cs.build_payload(it, TZ)) for gid, it in ITEMS.items()]
    events = {e.id: e for e in guild.events}
    await cog.sync_discord_events(guild, dict(ITEMS), payloads, events, TZ)
    if cog.can_write:
        await cog.import_discord_events(guild, events, TZ)
    guild.events = list(events.values())

def linked_pair():
    ITEMS["A"] = item("A", "Bash Scripting Workshop")
    start, end = (cs.parse_gcal_time(ITEMS["A"][k], TZ) for k in ("start", "end"))
    d = Event("Bash Scripting Workshop", "old text", "ENG2 205", start, end)
    return d, Guild([d])

def done(kind):
    return [c for c in CALLS if c[0] == kind]


async def test_first_contact_calendar_wins():
    d, g = linked_pair(); cog = new_cog(True)
    await sync(cog, g)
    assert d.description == "About it" and "A" in cog.state["snapshots"]

async def test_discord_rename_goes_to_calendar_without_echo():
    d, g = linked_pair(); cog = new_cog(True); await sync(cog, g)
    d.name = "Bash Scripting Workshop!"; CALLS.clear(); await sync(cog, g)
    assert done("gcal.patch")[0][2]["summary"] == "Bash Scripting Workshop!"
    assert not done("discord.edit")

async def test_read_only_reverts_discord_edits():
    d, g = linked_pair(); cog = new_cog(False); await sync(cog, g)
    d.location = "HEC 101"; CALLS.clear(); await sync(cog, g)
    assert d.location == "ENG2 205" and not any(c[0].startswith("gcal") for c in CALLS)

async def test_both_changed_calendar_wins():
    d, g = linked_pair(); cog = new_cog(True); await sync(cog, g)
    ITEMS["A"]["summary"] = "From calendar"; d.name = "From discord"; await sync(cog, g)
    assert d.name == "From calendar" and not done("gcal.patch")

async def test_calendar_flyer_becomes_cover():
    d, g = linked_pair(); cog = new_cog(True); await sync(cog, g)
    ITEMS["A"]["description"] = "About it\nFlyer: https://example.com/flyer.png"; CALLS.clear(); await sync(cog, g)
    assert any("image" in c[2] for c in done("discord.edit")) and d.description == "About it"

async def test_discord_cover_swaps_only_flyer_line():
    d, g = linked_pair(); cog = new_cog(True)
    ITEMS["A"]["description"] = "<b>Long rich description</b>\nFlyer: https://example.com/flyer.png"
    await sync(cog, g); await sync(cog, g)
    d.cover_image = Asset("officer-upload"); CALLS.clear(); await sync(cog, g)
    body = done("gcal.patch")[-1][2]["description"]
    assert "Long rich description" in body and "officer-upload" in body

async def test_discord_delete_deletes_calendar_event():
    d, g = linked_pair(); cog = new_cog(True); await sync(cog, g)
    g.events = []; CALLS.clear(); await sync(cog, g)
    assert ("gcal.delete", "A") in CALLS and not done("discord.create")

async def test_burst_of_deletions_is_blocked():
    for i, k in enumerate("BCDE"):
        ITEMS[k] = item(k, f"Event {k}", days=3 + i)
    g = Guild([]); cog = new_cog(True); await sync(cog, g)
    g.events = []; CALLS.clear(); await sync(cog, g)
    assert not done("gcal.delete")

async def test_unconfirmed_deletion_is_ignored():
    d, g = linked_pair(); cog = new_cog(True); await sync(cog, g)
    g.events = []; g.fail_lookups = True; CALLS.clear(); await sync(cog, g)
    assert not done("gcal.delete")

async def test_discord_made_event_added_to_calendar_once():
    human = Event("Study Night", "Bring snacks", "ENG2 205", NOW + datetime.timedelta(days=2),
                  NOW + datetime.timedelta(days=2, hours=2), creator=12345, cover="flyer1")
    g = Guild([human]); cog = new_cog(True); await sync(cog, g)
    gid = next(k for k, v in ITEMS.items() if v["summary"] == "Study Night")
    assert cog.state["event_map"][gid] == human.id and "Flyer: " in ITEMS[gid]["description"]
    CALLS.clear(); await sync(cog, g)
    assert not done("gcal.insert") and not done("discord.create")

async def test_read_only_recreates_deleted_discord_event():
    ITEMS["F"] = item("F", "GBM #3"); g = Guild([]); cog = new_cog(False); await sync(cog, g)
    g.events = []; CALLS.clear(); await sync(cog, g)
    assert done("discord.create") and not any(c[0].startswith("gcal") for c in CALLS)
