import datetime

from bot.cogs import officer_tools as ot
from tests.conftest import make_event


def test_room_in_calendar_counts_as_confirmed():
    assert ot.room_state(make_event(location="ENG2 205"), {}) == ("confirmed", "ENG2 205")
    assert ot.room_state(make_event(location="TBA"), {}) == ("none", None)

def test_request_becomes_confirmed_when_room_shows_up_in_calendar():
    rooms = {"g1": {"status": "requested", "room": "ENG2 102", "since": "2026-10-30"}}
    assert ot.room_state(make_event(location="TBA"), rooms)[0] == "requested"
    assert ot.room_state(make_event(location="ENGII 102"), rooms)[0] == "confirmed"

def test_denied_stays_denied():
    rooms = {"g1": {"status": "denied", "room": "HEC 101", "since": "2026-10-30"}}
    assert ot.room_state(make_event(location="TBA"), rooms) == ("denied", "HEC 101")

def test_clash_uses_requested_rooms_and_spelling_variants():
    a = make_event(name="Vulkan", location="TBA", gid="a")
    b = make_event(name="IoT", location="ENGII 102", gid="b")
    rooms = {"a": {"status": "requested", "room": "ENG2-102", "since": "2026-10-30"}}
    assert [e['name'] for e in ot.room_clashes(a, [a, b], rooms)] == ["IoT"]
    later = make_event(name="Later", location="ENG2 102", gid="c", days=6)
    assert ot.room_clashes(a, [a, later], rooms) == []

def test_events_check_shows_request_and_clash():
    a = make_event(name="Vulkan", location="TBA", gid="a")
    b = make_event(name="IoT", location="ENG2 102", gid="b")
    ot.save_json(ot.ROOMS_FILE, {"a": {"status": "requested", "room": "ENG2 102",
                                       "since": datetime.date.today().isoformat()}})
    lines = ot.check_events([a, b])
    assert "ENG2 102 requested since" in lines[0] and "same room as IoT" in lines[0]
