import datetime
import types

import discord
import pytest
from discord.ext import commands

from bot.officer import render, steps as st
from bot.officer import views
from bot.cogs import officer as officer_cog
from tests.officer_fixtures import NOW, event, snapshot, listing

OFFICER, EXEC, ADMIN, WORKSHOPS = 1449921163248533525, 1424610929424994477, 1499175237382570196, 1424671495560626216


# ---------- board and card ----------

def test_board_shows_open_events_with_phase_icons():
    upcoming, done_past = event(gid="a", days=5), event(gid="b", days=-5, name="Old")
    snap = snapshot([upcoming, done_past], store={"b": {"marks": {"recap": "done", "slides": "not needed"}}},
                    vtools={"b": listing(done_past, "vtools", **{"ieee-attending": 3, "guests-attending": 1})})
    rows = render.board_rows(snap, NOW)
    assert [e['id'] for e, _ in rows] == ["a"]                     # wrapped-up past event drops off
    line = render.board_line(*rows[0], snap)
    assert "Plan " in line and "Promote " in line and "Wrap up " in line and "to do" in line

def test_board_lists_knightconnect_orphans():
    snap = snapshot([], knightconnect_orphans=[{"name": "Trick or TECH", "startsOn": NOW.isoformat(),
                                                  "location": "EGN2-103", "link": "https://kc/1"}])
    text = render.board_embed(snap, []).description
    assert "Trick or TECH" in text and "EGN2-103" in text

def test_card_has_every_phase():
    e = event()
    embed = render.card_embed(e, st.event_steps(e, snapshot([e]), NOW), snapshot([e]))
    assert [f.name for f in embed.fields] == list(st.PHASES)
    assert "Owner" in embed.fields[0].value

def test_digest_splits_under_discord_limit():
    embeds = [render.build_embed(f"part {i}", ["x" * 1900]) for i in range(6)]
    batches = render.split_embeds(embeds)
    assert sum(len(b) for b in batches) == 6
    assert all(sum(render.embed_size(e) for e in b) <= 6000 for b in batches)


# ---------- schedule ----------

def test_digest_days():
    at = lambda d, h: datetime.datetime(2026, 11, d, h, tzinfo=NOW.tzinfo)
    assert officer_cog.digest_due(at(1, 10), None)          # Sunday
    assert officer_cog.digest_due(at(4, 11), "2026-11-01")  # Wednesday
    assert not officer_cog.digest_due(at(1, 9), None)
    assert not officer_cog.digest_due(at(2, 10), None)      # Monday
    assert not officer_cog.digest_due(at(1, 15), "2026-11-01")

def test_nudges_repeat_weekly():
    room = st.Step("room", "Plan", "Room", st.TODO)
    kc = st.Step("knightconnect", "Promote", "KnightConnect", st.TODO)
    sent = {"g1|room": "2026-11-01"}
    assert officer_cog.due_nudges({"g1": [room, kc]}, sent, datetime.date(2026, 11, 2)) == {"g1": [kc]}
    assert len(officer_cog.due_nudges({"g1": [room, kc]}, sent, datetime.date(2026, 11, 8))["g1"]) == 2


# ---------- officer-only ----------

class FakeResponse:
    def __init__(self):
        self.sent = None
    async def send_message(self, text, **kwargs):
        self.sent = text
    def is_done(self):
        return self.sent is not None

def interaction_with(roles):
    return types.SimpleNamespace(user=types.SimpleNamespace(id=1, roles=[types.SimpleNamespace(id=r) for r in roles]),
                                 response=FakeResponse())

@pytest.mark.parametrize("roles,allowed", [([OFFICER], True), ([EXEC], True), ([ADMIN], False), ([], False), ([WORKSHOPS], False)])
async def test_only_officers_can_use_officer_commands(roles, allowed, monkeypatch):
    monkeypatch.delenv("OFFICER_DIGEST_CHANNEL_ID", raising=False)
    monkeypatch.delenv("NUDGE_CHANNEL_ID", raising=False)
    bot = commands.Bot(command_prefix="!", intents=discord.Intents.none())
    cog = officer_cog.Officer(bot)
    monkeypatch.setattr(cog, "cog_load", lambda: _noop())
    await bot.add_cog(cog)
    cmds = [c for c in bot.tree.walk_commands() if isinstance(c, discord.app_commands.Command)]
    assert sorted(c.qualified_name for c in cmds) == ["officer chapter", "officer event", "officer events"]
    for command in cmds:
        interaction = interaction_with(roles)
        assert await command._check_can_run(interaction) is allowed
        if not allowed:
            assert interaction.response.sent == "These commands are for officers only."

async def _noop():
    return None

async def test_open_event_button_refuses_non_officers():
    button = views.OpenEventButton("g1")
    interaction = interaction_with([WORKSHOPS])
    await button.callback(interaction)
    assert interaction.response.sent == "Event cards are for officers only."

def test_card_menu_offers_every_action():
    keys = {key for key, _, _ in views.ACTIONS}
    for step in st.event_steps(event(), snapshot([event()]), NOW):
        for action in step.actions:
            assert action in keys or action.split(":")[0] == "mark", action

def test_posted_announcement_allows_only_written_mentions():
    allowed = views.mentions_in("# Hey <@&111>! ...")
    assert allowed.everyone is False and [r.id for r in allowed.roles] == [111]
    assert views.mentions_in("# Hey @everyone!").everyone is True
