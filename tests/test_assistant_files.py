import datetime
import types

from bot.assistant import agent, drive, notion
from bot.assistant.knowledge import Knowledge
from bot.assistant.tools_files import drive_id, notion_id, FILE_TOOLS

ASKER = 1


# ---------- ids and Notion formatting ----------

def test_ids_from_links():
    assert drive_id("https://drive.google.com/drive/folders/1AbCdEfGhIjKlMn?usp=sharing") == "1AbCdEfGhIjKlMn"
    assert drive_id("https://docs.google.com/document/d/1xYz_ABCdefGHIjk/edit") == "1xYz_ABCdefGHIjk"
    assert drive_id(" 1AbCdEfGhIjKlMn ") == "1AbCdEfGhIjKlMn"
    page = "https://www.notion.so/IOT-WorkShop-0123456789abcdef0123456789abcdef?pvs=4"
    assert notion_id(page) == "0123456789abcdef0123456789abcdef"
    assert notion_id("Face-0123456789abcdef0123456789abcdef".join(["https://www.notion.so/", ""])) == "0123456789abcdef0123456789abcdef"
    assert notion_id("40d10701-75fa-8172-94a2-002735d4e6ab") == "40d10701-75fa-8172-94a2-002735d4e6ab"

def test_markdown_to_notion_blocks():
    text = "# Plan\nIntro line\n- bullet\n1. first\n- [x] done\n- [ ] todo\n> quote\n---\n```\ncode here\n```"
    kinds = [b["type"] for b in notion.blocks_from_text(text)]
    assert kinds == ["heading_1", "paragraph", "bulleted_list_item", "numbered_list_item", "to_do", "to_do",
                     "quote", "divider", "code"]
    todo = notion.blocks_from_text("- [x] done")[0]["to_do"]
    assert todo["checked"] and todo["rich_text"][0]["text"]["content"] == "done"
    long = notion.rich("x" * 4500)
    assert [len(r["text"]["content"]) for r in long] == [2000, 2000, 500]

def test_property_values_by_type():
    assert notion.property_value("select", "Workshop") == {"select": {"name": "Workshop"}}
    assert notion.property_value("multi_select", "GPU, HPC") == {"multi_select": [{"name": "GPU"}, {"name": "HPC"}]}
    assert notion.property_value("date", "2026-11-13 to 2026-11-14") == {"date": {"start": "2026-11-13", "end": "2026-11-14"}}
    assert notion.property_value("checkbox", "yes") == {"checkbox": True}
    assert notion.property_value("number", "20") == {"number": 20.0}
    try:
        notion.property_value("relation", "x")
    except ValueError as e:
        assert "relation" in str(e)

def test_every_tool_has_a_handler():
    handlers = {name[2:] for name in dir(agent.Ask) if name.startswith("t_")}
    assert {t["name"] for t in agent.TOOLS} <= handlers
    assert agent.TOOLS[-1]["cache_control"] == {"type": "ephemeral"} and FILE_TOOLS[-1].get("cache_control") is None


# ---------- Discord files → Drive and Notion ----------

class Attachment:
    def __init__(self, filename, content_type="image/png", size=2048):
        self.filename, self.content_type, self.size = filename, content_type, size
    async def read(self):
        return b"png-bytes"

def message(mid, author, files, content=""):
    return types.SimpleNamespace(id=mid, attachments=files, author=types.SimpleNamespace(id=author, display_name=f"user{author}"),
                                 content=content, created_at=datetime.datetime(2026, 11, 3, 18, 0),
                                 jump_url=f"https://discord.com/channels/9/50/{mid}")

class Channel:
    name = "marketing"
    def __init__(self, messages):
        self.messages = messages
    def history(self, limit=200):
        async def gen():
            for m in self.messages:
                yield m
        return gen()
    async def fetch_message(self, mid):
        return next(m for m in self.messages if m.id == mid)

def make(messages):
    channel = Channel(messages)
    guild = types.SimpleNamespace(text_channels=[channel], threads=[], voice_channels=[])
    bot = types.SimpleNamespace(get_channel=lambda cid: channel)
    return agent.Ask(bot, guild, types.SimpleNamespace(id=ASKER, display_name="Eren"), Knowledge("data/k.db"))

async def test_find_files_then_save_to_drive_after_confirm(monkeypatch):
    flyer = message(101, ASKER, [Attachment("vulkan-flyer.png")], "flyer for vulkan")
    other = message(100, 7, [Attachment("meme.jpg", "image/jpeg")])
    ask = make([flyer, other])
    found = await ask.tool("find_discord_files", {"channel": "#Marketing", "from_asker": True})
    assert "vulkan-flyer.png" in found and "meme.jpg" not in found and "/101" in found

    monkeypatch.setattr(drive, "configured", lambda: True)
    monkeypatch.setattr(drive, "info", lambda fid: {"name": "Flyers"})
    uploads = []
    monkeypatch.setattr(drive, "upload", lambda data, name, mime, folder: uploads.append((data, name, mime, folder)) or "https://drive/x")
    reply = await ask.tool("save_discord_file_to_drive", {"message_url": flyer.jump_url,
                                                          "folder_id": "https://drive.google.com/drive/folders/1FlyerFolderId0"})
    assert reply.startswith("Queued:") and uploads == []                      # nothing until Confirm
    assert "**vulkan-flyer.png**" in ask.changes[0].summary and "**Flyers**" in ask.changes[0].summary
    result = await ask.changes[0].apply()
    assert uploads == [(b"png-bytes", "vulkan-flyer.png", "image/png", "1FlyerFolderId0")] and "https://drive/x" in result

async def test_picking_an_attachment_by_name_or_number():
    m = message(5, ASKER, [Attachment("a.png"), Attachment("b.pdf", "application/pdf")])
    ask = make([m])
    assert (await ask.attachment(m.jump_url, "2"))[0].filename == "b.pdf"
    assert (await ask.attachment(m.jump_url, "B.PDF"))[0].filename == "b.pdf"
    assert (await ask.attachment(m.jump_url))[0].filename == "a.png"

async def test_drive_tools_say_when_drive_isnt_connected(monkeypatch):
    monkeypatch.setattr(drive, "configured", lambda: False)
    ask = make([])
    try:
        await ask.tool("search_drive", {"text": "flyer"})
    except ValueError as e:
        assert "drive_login.py" in str(e)
    assert ask.changes == []

async def test_notion_changes_wait_for_confirm(monkeypatch):
    monkeypatch.setenv("NOTION_TOKEN", "x")
    calls = []
    async def read_page(pid):
        return ("Officer Meeting Nov 3", "u", "", "")
    async def create_page(*args):
        calls.append(("create", args))
        return "https://notion/new"
    async def archive(pid):
        calls.append(("archive", pid))
    monkeypatch.setattr(notion, "read_page", read_page)
    monkeypatch.setattr(notion, "create_page", create_page)
    monkeypatch.setattr(notion, "archive", archive)
    ask = make([])
    await ask.tool("create_notion_page", {"parent_id": "p1", "parent_type": "database", "title": "GBM #3",
                                          "properties": {"Date": "2026-11-19"}})
    await ask.tool("archive_notion_page", {"page_id": "p2"})
    assert calls == [] and "Date: 2026-11-19" in ask.changes[0].summary and "trash" in ask.changes[1].summary
    for change in ask.changes:
        await change.apply()
    assert calls[0][0] == "create" and calls[0][1][:3] == ("p1", "database", "GBM #3") and calls[1] == ("archive", "p2")
