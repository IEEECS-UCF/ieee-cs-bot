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
    async def send(self, content, embed=None, view=None, allowed_mentions=None, files=(), **kwargs):
        if self.fail:
            raise discord.HTTPException(types.SimpleNamespace(status=500, reason="boom"), "boom")
        self.sent.append((content, embed, view))
        self.files = [f.filename for f in files]
        return types.SimpleNamespace(id=60 + len(self.sent), jump_url=f"https://discord.com/review/{len(self.sent)}")
    def get_partial_message(self, mid):
        thread = self
        class Message:
            async def edit(self, **kwargs):
                thread.edits.append((mid, kwargs))
        return Message()

class Channel:
    def __init__(self):
        self.posts, self.files = [], []
    async def send(self, text, allowed_mentions=None, files=()):
        self.posts.append(text)
        self.files.append(files)
        return types.SimpleNamespace(jump_url="https://discord.com/announcements/1")

def client(thread, channel=None, flyer=None):
    async def event_post(e):
        return thread
    async def download_flyer(url):
        return flyer
    cog = types.SimpleNamespace(event_post=event_post, general_post=lambda: event_post(None),
                                cache=types.SimpleNamespace(snapshot=None))
    sync = types.SimpleNamespace(download_flyer=download_flyer)
    return types.SimpleNamespace(get_cog=lambda name: sync if name == "CalendarSync" else cog,
                                 get_partial_messageable=lambda tid: thread, get_channel=lambda cid: channel)

class Attachment:
    filename = "flyer.png"
    async def to_file(self):
        return "flyer-file"

class Interaction:
    def __init__(self, user, bot, attachments=()):
        self.user, self.client, self.said = types.SimpleNamespace(id=user), bot, []
        self.message = types.SimpleNamespace(attachments=list(attachments))
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


# ---------- the flyer goes with the announcement ----------

async def test_the_flyer_reviewers_saw_is_posted_with_the_announcement(monkeypatch):
    monkeypatch.setenv("ANNOUNCE_CHANNEL_ID", "9")
    thread, channel = Thread(), Channel()
    bot = client(thread, channel, flyer=b"png")
    async def redraw(fresh=False):
        pass
    draft = views.DraftView(types.SimpleNamespace(redraw=redraw), event(), "Hello")
    await draft.send_once(Interaction(AUTHOR, bot), "Hello")
    assert thread.files == ["flyer.png"] and thread.sent[0][1].image.url == "attachment://flyer.png"
    await views.approve(Interaction(OTHER, bot, attachments=[Attachment()]), "g1", "announcement")
    assert channel.files == [["flyer-file"]]

async def test_reviewers_are_told_when_there_is_no_flyer(monkeypatch):
    bot = client(Thread(), Channel(), flyer=None)
    await review.open_review(bot, event(), "announcement", "Hello", AUTHOR, view_for=views.review_buttons)
    assert "isn't a downloadable image" in review.message_parts("x", "announcement", review.get("g1", "announcement"))[0]
    await review.open_review(bot, event(flyer=None), "announcement", "Hello", AUTHOR, view_for=views.review_buttons)
    assert "No flyer" in review.message_parts("x", "announcement", review.get("g1", "announcement"))[0]


# ---------- Draft with AI ----------

async def test_an_ai_draft_replaces_the_draft_and_still_needs_review(monkeypatch):
    from bot.utils import ai_draft
    monkeypatch.setenv("ANTHROPIC_API_KEY", "test")
    seen = {}
    async def fake_draft(e, description, link, opener, examples):
        seen.update(opener=opener, examples=examples)
        return "# Hey <@&111>!\nAI text", None
    monkeypatch.setattr(ai_draft, "draft", fake_draft)
    edits = []
    class Response:
        async def defer(self):
            pass
    async def edit_original_response(content, view):
        edits.append(content)
    async def redraw(fresh=False):
        pass
    view = views.DraftView(types.SimpleNamespace(redraw=redraw), event(), "Plain", "https://discord.com/events/1/2", "<@&111>")
    interaction = types.SimpleNamespace(response=Response(), edit_original_response=edit_original_response,
                                        client=types.SimpleNamespace(get_channel=lambda cid: None),
                                        user=types.SimpleNamespace(id=AUTHOR))
    await view.with_ai.callback(interaction)
    assert view.draft == "# Hey <@&111>!\nAI text" and view.ai and seen["opener"] == "<@&111>"
    assert "written by Claude" in edits[0]
    bot = client(Thread(), Channel())
    await view.send_once(Interaction(AUTHOR, bot), view.draft)
    saved = review.get("g1", "announcement")
    assert saved["ai"] and saved["status"] == review.PENDING and "with AI" in review.status_line("announcement", saved)
    assert not review.edited(saved, "changed", OTHER)["ai"]

def test_ai_examples_are_the_chosen_posts():
    from bot.utils import ai_draft
    posts = ai_draft.example_posts()
    assert len(posts) == 10 and posts[0].startswith("# Hey @everyone!")
    assert "Discord markdown" in ai_draft.SYSTEM
    examples, details = ai_draft.prompt(["Example post"], ai_draft.details(event(), "Learn bash.", "https://d/1", "<@&111>"))
    assert "Example post" in examples["text"] and examples["cache_control"] == {"type": "ephemeral"}
    assert "Open with this mention: <@&111>" in details["text"] and "Room: ENG2 205" in details["text"]
    assert "cache_control" not in details                       # only the stable part is cached


# ---------- general announcements (/officer announce) ----------

class Image:
    filename = "poster.jpg"
    async def read(self):
        return b"jpg"

async def test_a_general_announcement_needs_another_officer_too(monkeypatch):
    monkeypatch.setenv("ANNOUNCE_CHANNEL_ID", "9")
    thread, channel = Thread(), Channel()
    bot = client(thread, channel)
    view = views.GeneralDraftView("Office hours moved to Friday!", Image())
    me = Interaction(AUTHOR, bot)
    me.id = 555
    assert (await view.send_once(me, view.draft)).startswith("Sent for review:")
    assert channel.posts == [] and thread.files == ["poster.jpg"]
    assert thread.sent[0][0].startswith("📝 **General announcement**")
    assert thread.sent[0][1].image.url == "attachment://poster.jpg"
    saved = review.get("general_555", "general")
    assert saved["status"] == review.PENDING and saved["editor"] == AUTHOR
    mine = Interaction(AUTHOR, bot)
    await views.approve(mine, "general_555", "general")
    assert "another officer" in mine.said[0] and channel.posts == []
    await views.approve(Interaction(OTHER, bot, attachments=[Attachment()]), "general_555", "general")
    assert channel.posts == ["Office hours moved to Friday!"] and channel.files == [["flyer-file"]]
    assert "marks" not in EventStore().get("general_555")             # not an event's announcement step

async def test_ai_writes_a_general_announcement_from_notes(monkeypatch):
    from bot.utils import ai_draft
    monkeypatch.setenv("ANTHROPIC_API_KEY", "test")
    notes_seen = []
    async def fake(notes, examples):
        notes_seen.append(notes)
        return f"Post #{len(notes_seen)}", None
    monkeypatch.setattr(ai_draft, "draft_from_notes", fake)
    class Response:
        async def defer(self):
            pass
    shown = []
    async def edit_original_response(content, view):
        shown.append(content)
    interaction = types.SimpleNamespace(response=Response(), edit_original_response=edit_original_response,
                                        client=types.SimpleNamespace(get_channel=lambda cid: None),
                                        user=types.SimpleNamespace(id=AUTHOR))
    view = views.GeneralDraftView("gbm moved to thu, bring laptops")
    await view.with_ai.callback(interaction)
    await view.with_ai.callback(interaction)                          # try again: same notes, not the last draft
    assert notes_seen == ["gbm moved to thu, bring laptops"] * 2 and view.draft == "Post #2" and view.ai
    assert "written by Claude" in shown[-1]
    assert "Office" not in ai_draft.from_notes("gbm moved") and "gbm moved" in ai_draft.from_notes("gbm moved")


# ---------- emergency posts ----------

async def test_an_emergency_post_goes_out_now_and_leaves_a_record(monkeypatch):
    monkeypatch.setenv("ANNOUNCE_CHANNEL_ID", "9")
    thread, channel = Thread(), Channel()
    bot = client(thread, channel, flyer=b"png")
    async def redraw(fresh=False):
        pass
    await review.open_review(bot, event(), "announcement", "Old draft", OTHER, view_for=views.review_buttons)
    draft = views.DraftView(types.SimpleNamespace(redraw=redraw), event(), "Room changed to ENG1 101!")
    url, why = await draft.send_emergency(Interaction(AUTHOR, bot), "Room changed to ENG1 101!", "Room moved an hour before")
    assert url and channel.posts == ["Room changed to ENG1 101!"] and len(channel.files[0]) == 1
    notice = thread.sent[-1][0]
    assert "🚨 Posted by <@1> without review" in notice and "Room moved an hour before" in notice
    saved = review.get("g1", "announcement")
    assert saved["status"] == review.POSTED and saved["emergency"] == "Room moved an hour before"
    assert EventStore().get("g1")["marks"]["announcement"] == "done"
    assert any("Not needed anymore" in kw.get("content", "") for _, kw in thread.edits)   # old review retired

async def test_an_emergency_general_post(monkeypatch):
    monkeypatch.setenv("ANNOUNCE_CHANNEL_ID", "9")
    thread, channel = Thread(), Channel()
    bot = client(thread, channel)
    view = views.GeneralDraftView("Building closed today, meeting is on Discord", Image())
    me = Interaction(AUTHOR, bot)
    me.id = 777
    url, why = await view.send_emergency(me, view.draft, "Campus closure")
    assert url and channel.posts == ["Building closed today, meeting is on Discord"]
    assert review.get("general_777", "general")["emergency"] == "Campus closure"
    assert "Campus closure" in thread.sent[-1][0]

async def test_nothing_posts_in_an_emergency_while_posting_is_off(monkeypatch):
    monkeypatch.delenv("ANNOUNCE_CHANNEL_ID", raising=False)
    monkeypatch.delenv("REMINDER_CHANNEL_ID", raising=False)
    view = views.GeneralDraftView("x")
    me = Interaction(AUTHOR, client(Thread(), Channel()))
    me.id = 1
    url, why = await view.send_emergency(me, "x", "because")
    assert url is None and "turned off" in why
