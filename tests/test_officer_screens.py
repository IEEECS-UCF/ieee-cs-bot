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


# ---------- nudge posts in a forum that requires tags ----------

TAGS = [types.SimpleNamespace(id=i, name=n) for i, n in enumerate(["workshop", "project", "prodev", "general"])]

def test_nudge_posts_get_a_tag_by_kind():
    assert officer_cog.forum_tag(TAGS, event(name="Bash Scripting Workshop")).name == "workshop"
    assert officer_cog.forum_tag(TAGS, event(name="Resume Review Night")).name == "prodev"
    assert officer_cog.forum_tag(TAGS, event(name="Board Game Night")).name == "general"
    assert officer_cog.forum_tag(TAGS[:2], event(name="TechX")).name == "workshop"   # no 'general': first tag
    assert officer_cog.forum_tag([], event(name="TechX")) is None

class Forum(discord.ForumChannel):
    def __init__(self, threads=(), archived=()):
        self.made, self._open, self._archived = [], list(threads), list(archived)
    available_tags = TAGS
    @property
    def threads(self):
        return self._open
    async def archived_threads(self, limit=None):
        for t in self._archived:
            yield t
    async def create_thread(self, name, content, applied_tags):
        self.made.append((name, [t.name for t in applied_tags]))
        return types.SimpleNamespace(thread=types.SimpleNamespace(id=99))

async def test_new_nudge_post_carries_the_tag():
    forum = Forum()
    cog = officer_cog.Officer.__new__(officer_cog.Officer)
    state = {"threads": {}}
    thread = await cog.event_thread(forum, event(gid="g1", name="Intro to gem5"), state)
    assert thread.id == 99 and state["threads"] == {"g1": 99}
    assert forum.made[0][1] == ["workshop"]

def post(i, name, archived=False):
    async def edit(archived):
        post.unarchived.append(i)
    return types.SimpleNamespace(id=i, name=name, archived=archived, edit=edit)
post.unarchived = []

async def test_officers_existing_post_is_used_instead_of_a_new_one():
    recent = discord.utils.time_snowflake(discord.utils.utcnow())
    officers = post(recent - 10, "TechX ", archived=True)
    lost = post(recent, "TechX (Sat Nov 14)")
    old = post(1, "TechX", archived=True)                      # years ago: another TechX
    forum = Forum(threads=[lost], archived=[officers, old])
    cog = officer_cog.Officer.__new__(officer_cog.Officer)
    state = {"threads": {}}
    thread = await cog.event_thread(forum, event(gid="t1", name="TechX"), state)
    assert thread is officers and state["threads"] == {"t1": officers.id}
    assert forum.made == [] and post.unarchived == [officers.id]


# ---------- each event post keeps one status message up to date ----------

class Thread:
    """A post whose first message is the bot's (or an officer's, with ours=False)."""
    def __init__(self, id, ours=True):
        self.id, self.ours, self.edits, self.sent = id, ours, [], []
    def get_partial_message(self, mid):
        thread = self
        class Message:
            async def edit(self, content, view, allowed_mentions):
                if mid == thread.id and not thread.ours:
                    raise discord.Forbidden(types.SimpleNamespace(status=403, reason="Forbidden"), "not ours")
                thread.edits.append((mid, content))
        return Message()
    async def send(self, content, view=None, allowed_mentions=None):
        self.sent.append(content)
        return types.SimpleNamespace(id=1000 + len(self.sent))

class FixedNow(datetime.datetime):
    @classmethod
    def now(cls, tz=None):
        return NOW

def nudge_cog(monkeypatch, tmp_path, snap, thread):
    monkeypatch.setattr(officer_cog, "NUDGES_FILE", str(tmp_path / "nudges.json"))
    monkeypatch.setattr(officer_cog, "datetime", types.SimpleNamespace(
        datetime=FixedNow, date=datetime.date, timedelta=datetime.timedelta))
    cog = officer_cog.Officer.__new__(officer_cog.Officer)
    cog.bot = types.SimpleNamespace(get_channel=lambda i: types.SimpleNamespace(guild=types.SimpleNamespace(id=5)))
    cog.nudge_channel_id = 1
    async def get(fresh=False):
        return snap
    cog.cache = types.SimpleNamespace(get=get)
    async def event_thread(channel, e, state):
        state["threads"][e['id']] = thread.id
        return thread
    cog.event_thread = event_thread
    return cog

async def test_status_is_edited_and_the_owner_pinged_once(monkeypatch, tmp_path):
    snap, thread = snapshot([event(gid="g1", days=5)]), Thread(77)
    cog = nudge_cog(monkeypatch, tmp_path, snap, thread)
    await cog.send_nudges_if_due(force=True)
    assert [mid for mid, _ in thread.edits] == [77]                  # the post's first message
    assert "Still open:" in thread.edits[0][1]
    assert len(thread.sent) == 1 and "/5/77/77" in thread.sent[0]   # the ping links to the status
    await cog.send_nudges_if_due()                                   # same day, nothing changed
    assert len(thread.edits) == 1 and len(thread.sent) == 1
    snap.events[0]['name'] = "Bash Scripting Workshop II"           # something changed: edit, no ping
    await cog.send_nudges_if_due()
    assert len(thread.edits) == 2 and "Workshop II" in thread.edits[1][1] and len(thread.sent) == 1

async def test_officers_post_gets_its_own_status_message(monkeypatch, tmp_path):
    thread = Thread(77, ours=False)
    cog = nudge_cog(monkeypatch, tmp_path, snapshot([event(gid="g1", days=5)]), thread)
    await cog.send_nudges_if_due(force=True)
    assert thread.edits == [] and thread.sent[0].startswith("**Bash Scripting Workshop**")
    assert "/5/77/1001" in thread.sent[1]                            # the ping links to the new status

def test_status_says_when_nothing_is_open():
    e = event()
    assert render.nudge_status(e, [], None).endswith("✅ Nothing open right now.")
    assert "No owner yet." in render.nudge_status(e, [], None)
