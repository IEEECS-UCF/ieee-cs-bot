import datetime
import json
import os
import types

import discord

from bot.officer import sources, views, data
from bot.officer.data import EventStore
from bot.cogs import event_reminders
from bot.utils import event_text
from tests.officer_fixtures import NOW, event, snapshot, listing


# ---------- matching listings to events ----------

def test_only_workshops_are_matched_to_slide_repos():
    gbm = event(gid="gbm", name="Fall GBM #2", days=-10)
    workshop = event(gid="emb", name="Intro to Embedded", days=-20)
    repos = [{"name": "Intro-to-GPU-Kernels", "html_url": "u1", "created_at": (gbm["start"] + datetime.timedelta(days=2)).isoformat()},
             {"name": "Intro-to-Embedded", "html_url": "u2", "created_at": NOW.isoformat()}]
    found = sources.match_slides([gbm, workshop], repos)
    assert found == {"emb": {"name": "Intro-to-Embedded", "url": "u2"}}   # the GBM doesn't take the repo made two days later

def test_a_listing_needs_a_similar_name_not_just_the_same_time():
    gem5 = event(gid="gem5", name="Intro to gem5")
    vulkan = listing(event(name="Vulkan Workshop"), "vtools", id=1)
    found = sources.assign_listings([gem5], lambda e: [vulkan], "start-time", "title")
    assert found == {}

def test_each_listing_goes_to_one_event():
    gem5, vulkan = event(gid="gem5", name="Intro to gem5"), event(gid="vulkan", name="Vulkan Workshop")
    listed = listing(vulkan, "vtools", id=1, title="Vulkan Workshop (co-hosted)")
    found = sources.assign_listings([gem5, vulkan], lambda e: [listed], "start-time", "title")
    assert list(found) == ["vulkan"]

def test_best_fit_wins_and_hand_links_are_taken():
    a, b = event(gid="a", name="GBM #3"), event(gid="b", name="GBM #3 Social", days=10)
    la = listing(a, "knightconnect", id=5)
    lb = listing(b, "knightconnect", id=6, startsOn=(b["start"] + datetime.timedelta(hours=4)).isoformat())
    found = sources.assign_listings([a, b], lambda e: [la, lb], "startsOn", "name")
    assert found["a"]["id"] == 5 and found["b"]["id"] == 6
    assert "a" not in sources.assign_listings([a], lambda e: [la], "startsOn", "name", taken=["5"])


# ---------- announcement drafts ----------

class Channel:
    def __init__(self, fail):
        self.fail, self.sent = fail, []
    async def send(self, text, **kwargs):
        if self.fail:
            raise discord.HTTPException(types.SimpleNamespace(status=500, reason="boom"), "boom")
        self.sent.append(text)
        return types.SimpleNamespace(jump_url="https://discord.com/x")

async def test_a_failed_post_can_be_retried():
    channel = Channel(fail=True)
    async def redraw(fresh=False):
        pass
    card = types.SimpleNamespace(gid="g1", redraw=redraw)
    draft = views.DraftView(card, "Hello", 123)
    interaction = types.SimpleNamespace(client=types.SimpleNamespace(get_channel=lambda cid: channel),
                                        user=types.SimpleNamespace(id=1))
    assert "Nothing was posted" in await draft.post_once(interaction, "Hello")
    assert not draft.posted and EventStore().get("g1") == {}
    channel.fail = False
    assert (await draft.post_once(interaction, "Hello")).startswith("Posted:")
    assert channel.sent == ["Hello"] and EventStore().get("g1")["marks"]["announcement"] == "done"
    assert await draft.post_once(interaction, "Hello") == "Already posted from this draft."


# ---------- new events past the board's window ----------

async def test_far_off_new_event_still_opens_its_card(monkeypatch):
    start = NOW + datetime.timedelta(days=100)
    end = start + datetime.timedelta(hours=2)
    item = {"id": "far", "summary": "Spring Kickoff", "start": {"dateTime": start.isoformat()},
            "end": {"dateTime": end.isoformat()}}
    monkeypatch.setattr(views, "insert_event", lambda cal, body: item)
    opened = []
    async def fake_open(cog, interaction, gid):
        opened.append(cog.cache.snapshot.event(gid)["name"])
    monkeypatch.setattr(views, "open_card", fake_open)
    snap = snapshot([])
    async def get(fresh=False):
        return snap
    cog = types.SimpleNamespace(cache=types.SimpleNamespace(get=get, snapshot=snap))
    v = {"name": "Spring Kickoff", "date": "", "time": "", "room": "", "description": ""}
    preview = views.NewEventPreview(cog, v, start, end)
    said = {}
    async def edit_original_response(**kwargs):
        said.update(kwargs)
    async def edit_message(**kwargs):
        pass
    interaction = types.SimpleNamespace(user=types.SimpleNamespace(id=1), edit_original_response=edit_original_response,
                                        response=types.SimpleNamespace(edit_message=edit_message),
                                        client=types.SimpleNamespace(get_cog=lambda name: None))
    await preview.create(interaction, None)
    assert opened == ["Spring Kickoff"] and "joins `/officer events` around" in said["content"]


# ---------- data files ----------

def test_a_corrupt_file_is_moved_aside_not_overwritten():
    with open("data/events.json", "w") as f:
        f.write('{"g1": {"owner": 4')
    assert EventStore().all() == {}
    aside = [n for n in os.listdir("data") if n.startswith("events.json.corrupt-")]
    assert len(aside) == 1 and open(os.path.join("data", aside[0])).read().startswith('{"g1"')


# ---------- one keyword list ----------

def test_every_feature_reads_the_same_keywords():
    assert [(k, w) for k, _, w in event_reminders.ROLE_KEYWORDS] == event_text.ROLE_WORDS
    tailgate = {"name": "Homecoming Tailgate"}
    assert event_text.kind(tailgate) == "socials"
    assert event_reminders.role_for("Homecoming Tailgate") == 222
