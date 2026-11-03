import datetime
import types


from bot.assistant import agent
from bot.assistant.knowledge import Knowledge, fts_query, chunks
from bot.cogs import assistant as cog
from bot.officer.data import EventStore
from tests.officer_fixtures import event, snapshot

ASKER, OTHER = 1, 2


# ---------- the knowledge cache ----------

def test_search_finds_ranks_and_filters():
    k = Knowledge("data/k.db")
    k.put("discord:1", "discord", "#ops-chat", "Eren", "2026-11-01", "u1", "Vulkan workshop needs a room, try ENG2 205")
    k.put("discord:2", "discord", "#general", "Sam", "2026-11-02", "u2", "anyone going to the vulkan thing?")
    k.put_document("notion", "p1", "Room booking guide", "u3", "2026-10-01", "To book a room email the CECS office. " * 3)
    hits = k.search("what room is the Vulkan workshop in?")
    assert hits[0]["url"] == "u1"                                      # more matching words ranks first
    assert [h["url"] for h in k.search("room", source="notion")] == ["u3"]
    assert k.search("!!! ???") == [] and k.search('"unclosed OR (') == []   # punctuation can't break the query
    k.put("discord:1", "discord", "#ops-chat", "Eren", "2026-11-01", "u1", "edited: room is ENG1 101")
    assert "ENG1" in k.search("room ENG1")[0]["text"] and len(k.search("Vulkan")) == 1
    k.remove("discord:2")
    assert k.search("anyone going") == []
    k.put_document("notion", "p1", "Room booking guide", "u3", "2026-10-02", "Rooms are booked in 25Live now.")
    assert len(k.search("CECS office")) == 0 and k.search("25Live")[0]["place"] == "Room booking guide"
    assert k.counts() == {"discord": 1, "notion": 1}

def test_query_and_chunks():
    assert fts_query("Where's the GPU kernels repo?") == '"where" OR "the" OR "gpu" OR "kernels" OR "repo"'
    parts = chunks("x" * 3500)
    assert len(parts) == 3 and all(len(p) <= 1500 for p in parts)
    assert chunks("") == [] and chunks("short") == ["short"]


# ---------- Discord messages as entries ----------

def message(content="hi", channel=None, embeds=(), attachments=()):
    return types.SimpleNamespace(
        id=10, content=content, embeds=list(embeds), attachments=list(attachments),
        channel=channel or types.SimpleNamespace(name="ops-chat"), author=types.SimpleNamespace(display_name="Eren"),
        created_at=datetime.datetime(2026, 11, 3, tzinfo=datetime.timezone.utc), jump_url="https://discord.com/channels/1/2/10")

def test_messages_become_entries():
    row = cog.entry(message("Room for Vulkan is ENG2 205", attachments=[types.SimpleNamespace(filename="flyer.png")]))
    assert row[0] == "discord:10" and row[2] == "#ops-chat" and row[3] == "Eren"
    assert "ENG2 205" in row[6] and "[attachment: flyer.png]" in row[6]
    embed = types.SimpleNamespace(title="Reminder", description="Bash is today")
    assert "Bash is today" in cog.entry(message("", embeds=[embed]))[6]
    assert cog.entry(message("")) is None

def test_only_officer_channels_get_answers():
    guild = types.SimpleNamespace(default_role="everyone")
    public = types.SimpleNamespace(permissions_for=lambda role: types.SimpleNamespace(view_channel=True))
    private = types.SimpleNamespace(permissions_for=lambda role: types.SimpleNamespace(view_channel=False))
    assert cog.officer_only(private, guild) and not cog.officer_only(public, guild)


# ---------- the agent ----------

class Officer:
    def __init__(self, snap):
        async def get(fresh=False):
            return snap
        self.cache = types.SimpleNamespace(get=get)

def make_ask(snap):
    synced = []
    sync = types.SimpleNamespace(sync_soon=lambda s: synced.append(s))
    bot = types.SimpleNamespace(get_cog=lambda name: Officer(snap) if name == "Officer" else sync)
    guild = types.SimpleNamespace(get_member=lambda uid: types.SimpleNamespace(display_name=f"user{uid}"))
    asker = types.SimpleNamespace(id=ASKER, display_name="Eren")
    return agent.Ask(bot, guild, asker, Knowledge("data/k.db")), synced

async def test_changes_wait_for_confirm(monkeypatch):
    vulkan = event(gid="v1", name="Intro to Vulkan", location="TBA", days=10)
    ask, synced = make_ask(snapshot([vulkan]))
    patched = []
    monkeypatch.setattr(agent, "patch_event", lambda cal, gid, body: patched.append((gid, body)))
    reply = await ask.tool("set_room", {"event_id": "v1", "room": "ENG2  205", "status": "confirmed"})
    assert reply.startswith("Queued:") and patched == [] and EventStore().get("v1") == {}   # nothing yet
    assert "**ENG2 205**" in ask.changes[0].summary
    result = await ask.changes[0].apply()                              # what Confirm runs
    assert patched == [("v1", {"location": "ENG2 205"})] and synced == [5]
    assert EventStore().get("v1")["room"]["status"] == "confirmed" and "ENG2 205" in result

async def test_edit_mark_owner_and_deadline_are_queued(monkeypatch):
    bash = event(gid="b1", name="Bash Scripting Workshop", days=10)
    ask, _ = make_ask(snapshot([bash]))
    patched = []
    monkeypatch.setattr(agent, "patch_event", lambda cal, gid, body: patched.append(body))
    await ask.tool("edit_event", {"event_id": "b1", "time": "7:00 PM-9:00 PM"})
    await ask.tool("mark_step", {"event_id": "b1", "step": "flyer", "value": "not needed"})
    await ask.tool("set_owner", {"event_id": "b1", "user_id": "<@42>"})
    await ask.tool("add_deadline", {"date": "2026-12-01", "what": "Officer reports due"})
    assert len(ask.changes) == 4 and patched == []
    for change in ask.changes:
        await change.apply()
    assert patched[0]["start"]["dateTime"].endswith("19:00:00-05:00")
    record = EventStore().get("b1")
    assert record["marks"]["flyer"] == "not needed" and record["owner"] == 42
    assert "Nothing would change." == await ask.tool("edit_event", {"event_id": "b1"})

async def test_unknown_events_are_errors_not_changes():
    ask, _ = make_ask(snapshot([]))
    try:
        await ask.tool("set_room", {"event_id": "nope", "room": "x", "status": "confirmed"})
    except ValueError as e:
        assert "list_events" in str(e)
    assert ask.changes == []

async def test_reading_tools():
    vulkan = event(gid="v1", name="Intro to Vulkan", days=3)
    ask, _ = make_ask(snapshot([vulkan, event(gid="old", name="Old", days=-30)]))
    listed = await ask.tool("list_events", {})
    assert "v1 | Intro to Vulkan" in listed and "Old" not in listed
    assert "No matching" in await ask.tool("list_events", {"name_contains": "gem5"})
    assert "Steps:" in await ask.tool("event_details", {"event_id": "v1"})
    ask.knowledge.put("discord:5", "discord", "#ops-chat", "Eren", "2026-11-01", "u5", "Vulkan room is ENG2 205")
    assert "u5" in await ask.tool("search", {"query": "vulkan room"})


# ---------- the loop with a fake Claude ----------

def block(**kw):
    return types.SimpleNamespace(**kw)

def response(stop, content):
    usage = types.SimpleNamespace(input_tokens=10, cache_read_input_tokens=5, cache_creation_input_tokens=0, output_tokens=3)
    return types.SimpleNamespace(stop_reason=stop, content=content, usage=usage)

async def test_the_loop_runs_tools_then_answers(monkeypatch):
    vulkan = event(gid="v1", name="Intro to Vulkan", location="TBA", days=10)
    ask, _ = make_ask(snapshot([vulkan]))
    replies = [
        response("tool_use", [block(type="tool_use", id="t1", name="set_room",
                                    input={"event_id": "v1", "room": "ENG2 205", "status": "confirmed"})]),
        response("end_turn", [block(type="text", text="I'll set Intro to Vulkan's room to ENG2 205 once you confirm.")]),
    ]
    sent = []
    class Messages:
        async def create(self, **kw):
            sent.append(kw)
            return replies.pop(0)
    monkeypatch.setattr(agent.anthropic, "AsyncAnthropic",
                        lambda: types.SimpleNamespace(beta=types.SimpleNamespace(messages=Messages())))
    answer = await ask.run("can you set the room for the vulkan workshop to be egn 205")
    assert "once you confirm" in answer and len(ask.changes) == 1
    assert sent[0]["model"] == "claude-sonnet-5-5" and sent[0]["output_config"] == {"effort": "low"}
    assert sent[1]["messages"][-1]["content"][0]["content"].startswith("Queued:")
    assert ask.usage == {"input": 20, "cached": 10, "cache_write": 0, "output": 6}


# ---------- Confirm / Cancel ----------

async def test_only_the_asker_can_confirm():
    said = []
    async def send_message(text, ephemeral=False):
        said.append(text)
    view = cog.ConfirmChanges(ASKER, [])
    someone = types.SimpleNamespace(user=types.SimpleNamespace(id=OTHER),
                                    response=types.SimpleNamespace(send_message=send_message))
    assert await view.interaction_check(someone) is False and "Only the officer who asked" in said[0]
    me = types.SimpleNamespace(user=types.SimpleNamespace(id=ASKER))
    assert await view.interaction_check(me) is True
