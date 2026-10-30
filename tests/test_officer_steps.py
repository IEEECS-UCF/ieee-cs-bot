import datetime

import pytest

from bot.officer import steps as st
from bot.officer.data import SKIPPED
from tests.officer_fixtures import NOW, event, snapshot, listing


def by_key(steps):
    return {s.key: s for s in steps}


# ---------- timing ----------

def test_timed_moves_later_todo_overdue():
    due = datetime.date(2026, 11, 20)
    assert st.timed(datetime.date(2026, 11, 1), due) == st.LATER
    assert st.timed(datetime.date(2026, 11, 15), due) == st.TODO
    assert st.timed(datetime.date(2026, 11, 21), due) == st.OVERDUE


# ---------- plan ----------

def test_owner_defaults_and_hand_set(monkeypatch):
    monkeypatch.setattr(st, "DEFAULT_OWNERS", {"workshops": "1", "other": "2"})
    assert st.owner_of(event(), {}) == (1, False)
    assert st.owner_of(event(name="Fall GBM #3"), {}) == (2, False)
    assert st.owner_of(event(name="Board Game Night Social"), {}) == (None, False)
    assert st.owner_of(event(), {"owner": 42}) == (42, True)

@pytest.mark.parametrize("location,record,status", [
    ("ENG2 205", {}, "confirmed"),
    ("TBA", {}, "none"),
    ("TBA", {"room": {"status": "requested", "room": "ENG2 102", "since": "2026-11-01"}}, "requested"),
    ("ENGII 102", {"room": {"status": "requested", "room": "ENG2 102", "since": "2026-11-01"}}, "confirmed"),
    ("TBA", {"room": {"status": "denied", "room": "HEC 101", "since": "2026-11-01"}}, "denied"),
])
def test_room_state(location, record, status):
    assert st.room_state(event(location=location), record)[0] == status

def test_room_request_waiting_a_week_is_overdue():
    e = event(location="TBA", days=30)
    fresh = {"g1": {"room": {"status": "requested", "room": "ENG2 102", "since": "2026-11-01"}}}
    stale = {"g1": {"room": {"status": "requested", "room": "ENG2 102", "since": "2026-10-20"}}}
    assert by_key(st.event_steps(e, snapshot([e], fresh), NOW))["room"].state == st.TODO
    assert by_key(st.event_steps(e, snapshot([e], stale), NOW))["room"].state == st.OVERDUE

def test_missing_description_and_flyer():
    e = event(description="", flyer=None, days=5)
    s = by_key(st.event_steps(e, snapshot([e]), NOW))
    assert s["description"].state == st.OVERDUE and s["flyer"].state == st.OVERDUE
    skipped = snapshot([e], {"g1": {"marks": {"flyer": SKIPPED}}})
    assert by_key(st.event_steps(e, skipped, NOW))["flyer"].state == st.SKIP


# ---------- promote ----------

def test_listing_matches_mismatch_missing():
    e = event(location="ENGII 102")
    ok = snapshot([e], knightconnect={"g1": listing(e, "knightconnect")})
    assert by_key(st.event_steps(e, ok, NOW))["knightconnect"].state == st.DONE
    wrong = snapshot([e], knightconnect={"g1": listing(e, "knightconnect", location="EGN2-103")})
    step = by_key(st.event_steps(e, wrong, NOW))["knightconnect"]
    assert step.state == st.WARN and "EGN2-103" in step.detail
    assert by_key(st.event_steps(e, snapshot([e]), NOW))["vtools"].state == st.OVERDUE  # due 3 weeks before
    soon = event(days=25)                                                                # due in 4 days
    assert by_key(st.event_steps(soon, snapshot([soon]), NOW))["vtools"].state == st.TODO
    far = event(days=40)
    assert by_key(st.event_steps(far, snapshot([far]), NOW))["vtools"].state == st.LATER

def test_short_title_inside_long_title_matches():
    e = event(name="TechX")
    snap = snapshot([e], vtools={"g1": listing(e, "vtools", title="TechX 2026: Bare Metal")})
    assert by_key(st.event_steps(e, snap, NOW))["vtools"].state == st.DONE

def test_announcement_marked():
    e = event(days=10)                     # due a week before: 3 days from now
    assert by_key(st.event_steps(e, snapshot([e]), NOW))["announcement"].state == st.TODO
    late = event(days=5)
    assert by_key(st.event_steps(late, snapshot([late]), NOW))["announcement"].state == st.OVERDUE
    done = snapshot([e], {"g1": {"marks": {"announcement": "done"}}})
    assert by_key(st.event_steps(e, done, NOW))["announcement"].state == st.DONE


# ---------- run ----------

def test_checkin_due_two_days_out():
    soon, late, later = event(days=3, gid="a"), event(days=1, gid="c"), event(days=20, gid="b")
    snap = snapshot([soon, late, later])
    assert by_key(st.event_steps(soon, snap, NOW))["checkin"].state == st.TODO
    assert by_key(st.event_steps(late, snap, NOW))["checkin"].state == st.OVERDUE
    assert by_key(st.event_steps(later, snap, NOW))["checkin"].state == st.LATER

def test_reminder_states():
    tba = event(location="TBA", days=2)
    assert by_key(st.event_steps(tba, snapshot([tba]), NOW))["reminder"].state == st.WARN
    forced = snapshot([tba], {"g1": {"reminder": {"send": "always"}}})
    assert "posts" in by_key(st.event_steps(tba, forced, NOW))["reminder"].detail
    sent = snapshot([event()], discord_ids={"g1": 555}, reminders_sent={555})
    assert by_key(st.event_steps(event(), sent, NOW))["reminder"].state == st.DONE


# ---------- after the event ----------

def test_missed_pre_event_steps_after_start():
    past = event(days=-3, description="", flyer=None)
    s = by_key(st.event_steps(past, snapshot([past]), NOW))
    assert s["announcement"].state == st.SKIP and s["announcement"].detail == "missed"
    assert s["checkin"].state == st.SKIP

def test_wrap_up_steps():
    past = event(days=-2)
    s = by_key(st.event_steps(past, snapshot([past]), NOW))
    assert s["attendance"].state == st.TODO and "create the vTools event" in s["attendance"].detail
    assert s["slides"].state == st.TODO and s["recap"].state == st.TODO
    filed = snapshot([past], vtools={"g1": listing(past, "vtools", **{"ieee-attending": 20, "guests-attending": 14})},
                     slides={"g1": {"name": "Bash-Scripting"}}, store={"g1": {"marks": {"recap": "done"}}})
    s = by_key(st.event_steps(past, filed, NOW))
    assert (s["attendance"].state, s["slides"].state, s["recap"].state) == (st.DONE, st.DONE, st.DONE)
    assert not st.is_open(past, list(s.values()), NOW)

def test_wrap_up_waits_for_event_to_end():
    today = event(days=0)
    assert by_key(st.event_steps(today, snapshot([today]), NOW))["recap"].state == st.LATER

def test_socials_skip_slides():
    social = event(name="Board Game Night", days=-2)
    assert by_key(st.event_steps(social, snapshot([social]), NOW))["slides"].state == st.SKIP

def test_check_in_counts_show_on_attendance():
    past = event(days=-1)
    snap = snapshot([past], store={"g1": {"checkin": {"formId": "f", "url": "u"}}},
                    checkins={"g1": {"total": 30, "ieee": 18, "guests": 12, "notSure": 0, "heard": {}}})
    assert "18 IEEE, 12 guests" in by_key(st.event_steps(past, snap, NOW))["attendance"].detail

def test_every_step_has_an_action():
    for step in st.event_steps(event(), snapshot([event()]), NOW):
        assert step.actions, step.key


def test_recap_not_tracked_before_launch(monkeypatch):
    monkeypatch.setattr(st, "TRACKING_START", datetime.date(2026, 10, 30))
    old = event(days=-20)   # Oct 13
    assert by_key(st.event_steps(old, snapshot([old]), NOW))["recap"].state == st.SKIP
    recent = event(days=-1)
    assert by_key(st.event_steps(recent, snapshot([recent]), NOW))["recap"].state == st.TODO

def test_guide_to_counts_as_workshop():
    from bot.utils import event_text
    assert event_text.kind(event(name="The Beginners Guide to Linux")) == "workshops"
