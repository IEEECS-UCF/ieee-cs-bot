import json
import os

from bot.officer import data


def test_store_round_trip_and_cleanup():
    store = data.EventStore()
    store.set_owner("g1", 42)
    store.set_room("g1", "requested", "ENG2 102", by=1)
    store.set_link("knightconnect", "g1", {"id": "9", "link": "x"})
    store.mark("g1", "recap", "done")
    store.set_reminder("g1", send="always", ping="auto")
    record = store.get("g1")
    assert record["owner"] == 42 and record["room"]["status"] == "requested"
    assert record["reminder"] == {"send": "always"}
    store.set_owner("g1", None); store.set_room("g1", "clear", None, by=1)
    store.set_link("knightconnect", "g1", None); store.mark("g1", "recap", None)
    store.set_reminder("g1", send="auto")
    assert store.get("g1") == {}

def test_same_room_request_keeps_its_date():
    store = data.EventStore()
    store.set_room("g1", "requested", "ENG2 102", by=1)
    first = store.get("g1")["room"]["since"]
    store.update("g1", lambda r: r["room"].__setitem__("since", "2026-10-01"))
    store.set_room("g1", "requested", "ENG2 102", by=2, note="asked Pete")
    assert store.get("g1")["room"]["since"] == "2026-10-01" and first

def test_migration_from_old_files():
    data.save_json("data/owners.json", {"g1": 42})
    data.save_json("data/rooms.json", {"g1": {"status": "confirmed", "room": "TCH 151", "since": "2026-10-30"}})
    data.save_json("data/checkins.json", {"g1": {"formId": "f", "url": "u", "shortUrl": "s", "name": "x", "day": "y"}})
    data.save_json("data/wrapups.json", {"g1": {"vtools": "not needed", "slides_link": {"name": "r", "url": "u"}, "recap": "done"}})
    data.save_json("data/reminder_overrides.json", {"555": {"send": "always", "ping": "auto", "start": "x"}})
    assert data.migrate({555: "g1"}) == 5
    record = data.EventStore().get("g1")
    assert record["owner"] == 42 and record["room"]["room"] == "TCH 151"
    assert record["checkin"] == {"formId": "f", "url": "u", "shortUrl": "s", "linked": False}
    assert record["links"]["vtools"] == "not needed" and record["slides"]["name"] == "r"
    assert record["marks"]["recap"] == "done" and record["reminder"] == {"send": "always"}
    assert not os.path.exists("data/owners.json") and os.path.exists("data/owners.json.migrated")
    assert data.migrate({}) == 0   # only once

def test_deadlines_add_and_hide():
    shipped = data.load_deadlines()
    data.add_deadline(__import__("datetime").date(2026, 12, 1), "TEST")
    data.remove_deadline(data.deadline_key(*shipped[0]))
    names = [w for _, w in data.load_deadlines()]
    assert "TEST" in names and shipped[0][1] not in names


async def test_checkin_results_are_fetched_side_by_side(monkeypatch):
    """Each form takes ~1.5 s to read, so the refresh shouldn't wait on them one at a time."""
    import asyncio
    from bot.officer import sources
    running, peak = 0, 0
    async def results(form_id):
        nonlocal running, peak
        running += 1; peak = max(peak, running)
        await asyncio.sleep(0.01)
        running -= 1
        if form_id == "bad":
            raise RuntimeError("script down")
        return {"total": int(form_id[1:])}
    monkeypatch.setattr(sources.checkin_client, "configured", lambda: True)
    monkeypatch.setattr(sources.checkin_client, "results", results)
    events = [{"id": f"g{i}"} for i in range(6)] + [{"id": "nf"}, {"id": "gb"}]
    store = {f"g{i}": {"checkin": {"formId": f"f{i}"}} for i in range(6)} | {"gb": {"checkin": {"formId": "bad"}}}
    snap = sources.Snapshot(events=events, store=store)
    await sources._checkins(events, store, snap)
    assert peak == sources.CHECKIN_CONCURRENCY
    assert snap.checkins["g5"] == {"total": 5} and "nf" not in snap.checkins
    assert snap.checkins["gb"] == {"error": "script down"}
