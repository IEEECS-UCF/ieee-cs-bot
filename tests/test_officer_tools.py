import datetime
import json
import types

import discord
import pytest
from discord.ext import commands

from bot.cogs import officer_tools as ot
from tests.conftest import TZ, make_event

OFFICER, EXEC, ADMIN, WORKSHOPS = 1449921163248533525, 1424610929424994477, 1499175237382570196, 1424671495560626216


# ---------- officer-only access ----------

class FakeResponse:
    def __init__(self):
        self.sent = None
    async def send_message(self, text, **kwargs):
        self.sent = text

def interaction_with(roles):
    user = types.SimpleNamespace(id=1, roles=[types.SimpleNamespace(id=r) for r in roles])
    return types.SimpleNamespace(user=user, response=FakeResponse())

@pytest.mark.parametrize("roles,allowed", [([OFFICER], True), ([EXEC], True), ([ADMIN], False),
                                           ([], False), ([WORKSHOPS], False)])
async def test_every_officer_command_needs_officer_or_exec(roles, allowed, monkeypatch):
    monkeypatch.delenv("OFFICER_DIGEST_CHANNEL_ID", raising=False)
    monkeypatch.delenv("NUDGE_CHANNEL_ID", raising=False)
    bot = commands.Bot(command_prefix="!", intents=discord.Intents.none())
    await bot.add_cog(ot.OfficerTools(bot))
    cmds = [c for c in bot.tree.walk_commands() if isinstance(c, discord.app_commands.Command)]
    assert len(cmds) >= 20
    for command in cmds:
        interaction = interaction_with(roles)
        assert await command._check_can_run(interaction) is allowed, command.qualified_name
        if not allowed:
            assert interaction.response.sent == "These commands are for officers only."


# ---------- listings ----------

def kc_listing(event, **change):
    listing = {"id": 9, "name": event["name"], "startsOn": event["start"].isoformat(),
               "location": event["location"], "link": "https://kc/9"}
    listing.update(change)
    return listing

def test_listing_matches():
    e = make_event()
    state, note = ot.listing_status(e, kc_listing(e), "startsOn", "name", lambda m: m["location"])
    assert state == ot.DONE

def test_listing_room_and_time_differences():
    e = make_event(location="ENGII 102")
    wrong = kc_listing(e, location="EGN2-103", startsOn=(e["start"] + datetime.timedelta(hours=1)).isoformat())
    state, note = ot.listing_status(e, wrong, "startsOn", "name", lambda m: m["location"])
    assert state == "⚠️" and "EGN2-103" in note and "time" in note

def test_short_title_inside_long_title_is_fine():
    e = make_event(name="TechX")
    state, _ = ot.listing_status(e, kc_listing(e, name="TechX 2026: Bare Metal"), "startsOn", "name", lambda m: m["location"])
    assert state == ot.DONE

def test_missing_listing_only_flagged_when_close():
    assert ot.listing_status(make_event(days=5), None, "startsOn", "name", None)[0] == ot.TODO
    assert ot.listing_status(make_event(days=40), None, "startsOn", "name", None)[0] == ot.LATER
    assert ot.listing_status(make_event(), ot.SKIPPED, "startsOn", "name", None)[0] == ot.SKIP

def test_best_match_prefers_same_day_and_name():
    e = make_event()
    other_day = kc_listing(e, id=1, startsOn=(e["start"] + datetime.timedelta(days=1)).isoformat())
    same = kc_listing(e, id=2)
    assert ot.best_match(e, [other_day, same], "startsOn", "name")["id"] == 2


# ---------- deadlines ----------

def test_deadlines_add_and_hide():
    shipped = ot.load_deadlines()
    assert shipped
    ot.save_json(ot.DEADLINES_EXTRA_FILE, {"added": [{"date": "2026-12-01", "what": "TEST"}],
                                           "hidden": [ot.deadline_key(*shipped[0])]})
    names = [what for _, what in ot.load_deadlines()]
    assert "TEST" in names and shipped[0][1] not in names


# ---------- digest size ----------

def test_digest_splits_under_discord_limit():
    embeds = [ot.build_embed(f"part {i}", ["x" * 1900]) for i in range(6)]
    batches = ot.split_embeds(embeds)
    assert sum(len(b) for b in batches) == 6
    assert all(sum(ot.embed_size(e) for e in b) <= 6000 and len(b) <= 10 for b in batches)


# ---------- nudges ----------

def test_nudges_repeat_weekly_only():
    today = datetime.date(2026, 11, 2)
    items = {"g1": [("room", "no room"), ("knightconnect", "not listed")]}
    sent = {"g1|room": "2026-10-30"}
    assert ot.due_nudges(items, sent, today) == {"g1": [("knightconnect", "not listed")]}
    assert ot.due_nudges(items, sent, today + datetime.timedelta(days=5))["g1"][0][0] == "room"


# ---------- slides ----------

def test_slides_link_wins_then_name_then_date():
    linux = make_event(name="The Beginners Guide to Linux", gid="linux", days=-30)
    gbm = make_event(name="Fall GBM #2", gid="gbm", days=-8)
    embedded = make_event(name="Intro to Embedded", gid="emb", days=-20)
    created = lambda e, d: (e["start"] + datetime.timedelta(days=d)).isoformat()
    repos = [{"name": "Beginners-Guide-to-Linux", "created_at": created(linux, 6)},
             {"name": "Intro-to-GPU-Kernels", "created_at": created(gbm, 2)}]
    found = ot.match_slides([linux, gbm, embedded], repos)
    assert found["linux"]["name"] == "Beginners-Guide-to-Linux"
    assert found["gbm"]["name"] == "Intro-to-GPU-Kernels"
    assert "emb" not in found
    marks = {"emb": {"slides_link": {"name": "Slides", "url": "https://example.com"}}}
    assert ot.match_slides([embedded], repos, marks)["emb"]["linked"] is True


# ---------- announcement mentions ----------

def test_posted_announcement_allows_only_written_mentions():
    allowed = ot.mentions_in("# Hey <@&111>! ...")
    assert allowed.everyone is False and [r.id for r in allowed.roles] == [111]
    assert ot.mentions_in("# Hey @everyone!").everyone is True


# ---------- default owners ----------

def test_workshops_default_to_workshop_chair(monkeypatch):
    monkeypatch.setattr(ot, "DEFAULT_OWNERS", {"workshops": "711695730594676849"})
    workshop, gbm = make_event(name="Intro to gem5", gid="w"), make_event(name="Fall GBM #3", gid="g")
    assert ot.owner_of(workshop, {}) == (711695730594676849, False)
    assert ot.owner_of(gbm, {}) == (None, False)
    assert ot.owner_of(workshop, {"w": 42}) == (42, True)   # set by hand wins


# ---------- digest schedule ----------

def test_digest_posts_sunday_and_wednesday_once():
    at = lambda y, m, d, h: datetime.datetime(y, m, d, h, tzinfo=TZ)
    assert ot.digest_due(at(2026, 11, 1, 10), None)            # Sunday 10 AM
    assert ot.digest_due(at(2026, 11, 4, 11), "2026-11-01")    # Wednesday
    assert not ot.digest_due(at(2026, 11, 1, 9), None)         # too early
    assert not ot.digest_due(at(2026, 11, 2, 10), None)        # Monday
    assert not ot.digest_due(at(2026, 11, 1, 15), "2026-11-01")  # already posted today


def test_other_events_default_to_chair(monkeypatch):
    monkeypatch.setattr(ot, "DEFAULT_OWNERS", {"workshops": "1", "other": "2"})
    assert ot.owner_of(make_event(name="Fall GBM #3", gid="g"), {}) == (2, False)
    assert ot.owner_of(make_event(name="TechX", gid="t"), {}) == (2, False)
    assert ot.owner_of(make_event(name="Board Game Night Social", gid="s"), {}) == (None, False)
