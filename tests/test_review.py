import datetime
import types

import discord

from bot.officer import review, views, steps as st
from bot.officer.data import EventStore
from tests.officer_fixtures import NOW, event, snapshot

AUTHOR, OTHER = 1, 2


# ---------- fakes ----------

class Thread:
    def __init__(self, fail=False):
        self.id, self.fail, self.sent, self.edits = 50, fail, [], []
    async def send(self, content, embed=None, view=None, allowed_mentions=None, **kwargs):
        if self.fail:
            raise discord.HTTPException(types.SimpleNamespace(status=500, reason="boom"), "boom")
        self.sent.append((content, embed, view))
        return types.SimpleNamespace(id=60 + len(self.sent), jump_url=f"https://discord.com/review/{len(self.sent)}")
    def get_partial_message(self, mid):
        thread = self
        class Message:
            async def edit(self, **kwargs):
                thread.edits.append((mid, kwargs))
        return Message()

class Channel:
    def __init__(self):
        self.posts = []
    async def send(self, text, allowed_mentions=None):
        self.posts.append(text)
        return types.SimpleNamespace(jump_url="https://discord.com/announcements/1")

def client(thread, channel=None):
    async def event_post(e):
        return thread
    cog = types.SimpleNamespace(event_post=event_post, cache=types.SimpleNamespace(snapshot=None))
    return types.SimpleNamespace(get_cog=lambda name: cog, get_partial_messageable=lambda tid: thread,
                                 get_channel=lambda cid: channel)

class Interaction:
    def __init__(self, user, bot):
        self.user, self.client, self.said = types.SimpleNamespace(id=user), bot, []
        outer = self
        class Response:
            async def send_message(self, text, ephemeral=False):
                outer.said.append(text)
            async def defer(self, **kwargs):
                pass
        class Followup:
            async def send(self, text, ephemeral=False):
                outer.said.append(text)
        self.response, self.followup = Response(), Followup()


# ---------- announcement drafts ----------

async def test_a_draft_goes_to_review_and_never_straight_to_announcements(monkeypatch):
    monkeypatch.setenv("ANNOUNCE_CHANNEL_ID", "9")
    thread, channel = Thread(fail=True), Channel()
    async def redraw(fresh=False):
        pass
    draft = views.DraftView(types.SimpleNamespace(gid="g1", redraw=redraw), event(), "Hello <@&111>")
    bot = client(thread, channel)
    assert "Try again" in await draft.send_once(Interaction(AUTHOR, bot), "Hello <@&111>")
    assert not draft.sent and review.get("g1", "announcement") is None
    thread.fail = False
    assert (await draft.send_once(Interaction(AUTHOR, bot), "Hello <@&111>")).startswith("Sent for review:")
    assert channel.posts == []                                        # nothing public yet
    saved = review.get("g1", "announcement")
    assert saved["editor"] == AUTHOR and saved["status"] == review.PENDING and saved["message"] == [50, 61]
    assert "an officer other than <@1>" in thread.sent[0][0]
    assert [i.item.custom_id for i in thread.sent[0][2].children] == ["review:approve:announcement:g1",
                                                                       "review:edit:announcement:g1"]
    assert await draft.send_once(Interaction(AUTHOR, bot), "Hello") == "Already sent for review."

async def test_only_another_officer_can_approve_and_then_it_posts(monkeypatch):
    monkeypatch.setenv("ANNOUNCE_CHANNEL_ID", "9")
    thread, channel = Thread(), Channel()
    bot = client(thread, channel)
    await review.open_review(bot, event(), "announcement", "Hello", AUTHOR, view_for=views.review_buttons)
    mine = Interaction(AUTHOR, bot)
    await views.approve(mine, "g1", "announcement")
    assert "another officer" in mine.said[0] and channel.posts == []
    theirs = Interaction(OTHER, bot)
    await views.approve(theirs, "g1", "announcement")
    assert channel.posts == ["Hello"] and theirs.said[0].startswith("Posted:")
    saved = review.get("g1", "announcement")
    assert saved["status"] == review.POSTED and saved["approver"] == OTHER
    assert EventStore().get("g1")["marks"]["announcement"] == "done"
    again = Interaction(OTHER, bot)
    await views.approve(again, "g1", "announcement")
    assert channel.posts == ["Hello"] and "isn't waiting" in again.said[0]

async def test_nothing_posts_while_posting_is_turned_off(monkeypatch):
    monkeypatch.delenv("ANNOUNCE_CHANNEL_ID", raising=False)
    monkeypatch.delenv("REMINDER_CHANNEL_ID", raising=False)
    bot = client(Thread())
    await review.open_review(bot, event(), "announcement", "Hello", AUTHOR, view_for=views.review_buttons)
    theirs = Interaction(OTHER, bot)
    await views.approve(theirs, "g1", "announcement")
    assert "turned off" in theirs.said[0] and review.get("g1", "announcement")["status"] == review.PENDING

def test_whoever_edits_last_cannot_approve():
    first = {"text": "a", "editor": AUTHOR, "status": review.PENDING, "approver": None}
    assert not review.can_approve(first, AUTHOR)[0] and review.can_approve(first, OTHER)[0]
    changed = review.edited({**first, "status": review.APPROVED, "approver": OTHER}, "b", OTHER)
    assert changed["status"] == review.PENDING and changed["approver"] is None
    assert review.can_approve(changed, AUTHOR)[0] and not review.can_approve(changed, OTHER)[0]
    bots = {**first, "editor": None}                                  # the bot's own text: any officer
    assert review.can_approve(bots, AUTHOR)[0]


# ---------- day-of reminders ----------

def test_reminder_timeline():
    start = NOW.replace(hour=18, minute=30) + datetime.timedelta(days=2)
    review_from, post_at = review.review_window(start, 12, 9)
    assert review_from == start.replace(hour=12, minute=0) - datetime.timedelta(days=1)
    assert post_at == start.replace(hour=9, minute=0)
    act = lambda now, r: review.reminder_action(now, start, r, review_from, post_at)
    pending = {"for": start.isoformat(), "status": review.PENDING}
    approved = {**pending, "status": review.APPROVED}
    assert act(review_from - datetime.timedelta(minutes=1), None) is None
    assert act(review_from, None) == "open"
    assert act(review_from, {**pending, "for": "moved"}) == "open"     # event moved: review again
    assert act(review_from, {**pending, "status": review.CANCELLED}) == "open"
    assert act(post_at - datetime.timedelta(minutes=1), approved) is None
    assert act(post_at, approved) == "post"                           # only approved text posts
    assert act(post_at, pending) == "late"
    assert act(post_at, {**pending, "late_noted": True}) is None
    assert act(start, pending) == "missed" and act(start, approved) is None
    assert act(post_at, {**approved, "status": review.POSTED}) is None

def test_reminder_step_shows_the_review():
    e = event(days=1)
    waiting = snapshot([e], {"g1": {"reviews": {"reminder": {"for": e['start'].isoformat(), "status": "pending"}}}})
    step = {s.key: s for s in st.event_steps(e, waiting, NOW)}["reminder"]
    assert step.state == st.TODO and "waiting for an officer" in step.detail
    ok = snapshot([e], {"g1": {"reviews": {"reminder": {"for": e['start'].isoformat(), "status": "approved"}}}})
    assert "approved" in {s.key: s for s in st.event_steps(e, ok, NOW)}["reminder"].detail


async def test_reminder_loop_never_posts_without_approval(monkeypatch):
    from bot.cogs import event_reminders as er
    e = event(days=1)
    discord_event = types.SimpleNamespace(id=777, name=e['name'], location=e['location'], start_time=e['start'],
                                          status=discord.EventStatus.scheduled)
    thread, channel = Thread(), Channel()
    bot = client(thread, channel)
    officer = bot.get_cog("Officer")
    async def get(fresh=False):
        return snapshot([e])
    officer.cache = types.SimpleNamespace(get=get, snapshot=None)
    sync = types.SimpleNamespace(state={"event_map": {"g1": 777}})
    guild = types.SimpleNamespace(id=1, scheduled_events=[discord_event], get_channel=lambda cid: channel)
    bot.get_cog = lambda name: sync if name == "CalendarSync" else officer
    bot.get_guild = lambda gid: guild
    cog = er.EventReminders.__new__(er.EventReminders)
    cog.bot, cog.guild_id, cog.channel_id, cog.state = bot, 1, 9, {"sent": {}}
    monkeypatch.setenv("REMINDER_CHANNEL_ID", "9")

    clock = {}
    class Clock(datetime.datetime):
        @classmethod
        def now(cls, tz=None):
            return clock["now"]
    monkeypatch.setattr(er, "datetime", types.SimpleNamespace(datetime=Clock, timedelta=datetime.timedelta))
    async def at(when):
        clock["now"] = when
        await cog.run_reminders()

    await at(e['start'].replace(hour=11) - datetime.timedelta(days=1))
    assert thread.sent == [] and channel.posts == []
    await at(e['start'].replace(hour=12) - datetime.timedelta(days=1))   # up for review the day before
    assert len(thread.sent) == 1 and channel.posts == []
    await at(e['start'].replace(hour=9))                                 # 9 AM, not approved: tell the owner
    assert len(thread.sent) == 2 and "hasn't been approved" in thread.sent[1][0] and channel.posts == []
    await views.approve(Interaction(OTHER, bot), "g1", "reminder")
    await at(e['start'].replace(hour=9, minute=10))                      # approved: posts
    assert len(channel.posts) == 1 and "is TODAY" in channel.posts[0]
    await at(e['start'].replace(hour=9, minute=20))
    assert len(channel.posts) == 1 and review.get("g1", "reminder")["status"] == review.POSTED
