import pytest

from bot.utils import event_text as t
from bot.utils.event_text import place, same_place
from bot.utils.vtools_client import parse_event_id
from bot.utils.knightconnect_client import parse_event_id as kc_event_id
from bot.utils.checkin_client import parse_form_id
from tests.conftest import make_event


@pytest.mark.parametrize("a,b,same", [
    ("ENGII 102", "EGN2-103", False),            # the real Trick or TECH mismatch
    ("ENG2 205", "Engineering 2 Room 205", True),
    ("HEC-101", "L3Harris Corporation Engineering Center (HEC) 101AB", True),
    ("TCH 151", "Technology Commons 151", True),
    ("HS1 106", "HS1-272", False),
    ("BA1-126", "BA1 126", True),
    ("ENG1 186", "ENG2 186", False),
])
def test_same_place(a, b, same):
    assert same_place(a, b) is same

def test_place_parts():
    assert place("ENGII 102") == ("ENG2", "102")
    assert place("TBA") == (None, None)

def test_ids():
    assert parse_event_id("https://events.vtools.ieee.org/m/552721") == "552721"
    assert kc_event_id("https://knightconnect.campuslabs.com/engage/event/12931108") == "12931108"
    form = "19YdKAXRoW3vuiPcSwyBi1JeeIi7hRWpnCFW2Lg_jCFg"
    assert parse_form_id(f"https://docs.google.com/forms/d/{form}/edit") == form
    assert parse_form_id("https://docs.google.com/forms/d/e/1FAIpQLSe/viewform") is None  # share links have no ID

def test_vtools_copy_has_required_fields():
    text = t.copy_block(make_event(), "vtools")
    for field in ("Host Organizational Unit*", "Host Contact Email*", "Title*", "Category*", "End time*",
                  "Description*", "Tags/Keywords*", "City*", "Country/State*"):
        assert field in text
    assert "Engineering II" in text and "Room Number:** 205" in text

def test_category_follows_event_kind():
    assert "Nontechnical" in t.copy_block(make_event(name="Board Game Night Social"), "vtools")
    assert "Professional" in t.copy_block(make_event(name="LinkedIn Career Night"), "vtools")
    assert "Technical" in t.copy_block(make_event(), "vtools")

def test_flyer_line_stays_out_of_text():
    event = make_event(description="Learn bash.\nFlyer: https://example.com/f.png")
    assert "Flyer" not in t.plain_description(event)
    draft = t.announcement_draft(event, "https://discord.com/events/1/2", "<@&111>")
    assert draft.startswith("# Hey <@&111>!") and "Learn bash." in draft and "ENG2 205" in draft

def test_draft_without_role_uses_everyone():
    assert t.announcement_draft(make_event(name="Fall GBM #3"), "").startswith("# Hey @everyone!")
