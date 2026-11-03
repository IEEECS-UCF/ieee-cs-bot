import re
import asyncio
from urllib.parse import urlparse

from bot.assistant import drive, notion

# /officer ask tools for files: Discord attachments, Google Drive (Docs, Sheets)
# and Notion. Reads run right away; anything that changes something is queued
# with self.queue() and only happens when the asker clicks Confirm.

def tool(name, description, properties, required=()):
    return {"name": name, "description": description,
            "input_schema": {"type": "object", "properties": properties, "required": list(required)}}

S = {"type": "string"}
FILE_TOOLS = [
    # Discord
    tool("find_discord_files", "Recent messages with attachments in a channel or thread, newest first, with message links, file names and sizes. Use it to find e.g. 'the flyer I just posted in #marketing'.",
         {"channel": {**S, "description": "Channel or thread name, e.g. marketing"},
          "from_asker": {"type": "boolean", "description": "Only files the asker posted"},
          "limit": {"type": "integer", "minimum": 1, "maximum": 25}}, ["channel"]),
    # Drive reads
    tool("search_drive", "Search the chapter's Google Drive (ieeecsucf@gmail.com) by name or contents. Returns id | name | kind | last changed | link.",
         {"text": S, "kind": {"type": "string", "enum": ["any", "folder", "doc", "sheet", "slides", "pdf", "image"]},
          "folder_id": {**S, "description": "Only inside this folder"}}),
    tool("read_drive_file", "The text of a Google Doc, Sheet (as CSV), Slides deck or text file.", {"file_id": S}, ["file_id"]),
    tool("read_sheet", "Cells of a Google Sheet, as rows. Range like 'Attendance!A1:F50'; the first tab if left out.",
         {"sheet_id": S, "range": S}, ["sheet_id"]),
    # Drive writes
    tool("save_discord_file_to_drive", "Queue copying a file attached to a Discord message into a Drive folder.",
         {"message_url": S, "folder_id": S, "attachment": {**S, "description": "File name, or its number from find_discord_files; the first file if left out"},
          "new_name": S}, ["message_url", "folder_id"]),
    tool("create_drive_folder", "Queue creating a Drive folder (inside parent_folder_id, or at the top of My Drive).",
         {"name": S, "parent_folder_id": S}, ["name"]),
    tool("move_drive_file", "Queue moving a file or folder into another folder.", {"file_id": S, "folder_id": S}, ["file_id", "folder_id"]),
    tool("rename_drive_file", "Queue renaming a file or folder.", {"file_id": S, "name": S}, ["file_id", "name"]),
    tool("copy_drive_file", "Queue copying a file, optionally with a new name or into another folder.",
         {"file_id": S, "name": S, "folder_id": S}, ["file_id"]),
    tool("trash_drive_file", "Queue moving a file or folder to Drive's trash (restorable for 30 days).", {"file_id": S}, ["file_id"]),
    tool("share_drive_file", "Queue sharing a file or folder with someone by email (Google emails them the link).",
         {"file_id": S, "email": S, "role": {"type": "string", "enum": ["reader", "commenter", "writer"]}},
         ["file_id", "email", "role"]),
    tool("create_google_doc", "Queue creating a Google Doc with the given text.", {"title": S, "text": S, "folder_id": S}, ["title", "text"]),
    tool("append_to_google_doc", "Queue adding text to the end of a Google Doc.", {"doc_id": S, "text": S}, ["doc_id", "text"]),
    tool("append_sheet_rows", "Queue adding rows to the bottom of a Google Sheet tab.",
         {"sheet_id": S, "rows": {"type": "array", "items": {"type": "array", "items": S}}, "tab": S}, ["sheet_id", "rows"]),
    # Notion reads
    tool("search_notion", "Search the chapter's Notion for pages or databases by title. Returns id | kind | title | link | last edited.",
         {"text": S, "kind": {"type": "string", "enum": ["any", "page", "database"]}}),
    tool("read_notion_page", "A Notion page's properties and full text (child pages and databases are listed with their ids).",
         {"page_id": S}, ["page_id"]),
    tool("query_notion_database", "A Notion database's columns and rows (each row starts with its page id), optionally only rows containing some text.",
         {"database_id": S, "contains": S}, ["database_id"]),
    # Notion writes
    tool("create_notion_page", "Queue creating a Notion page under a page, or a row in a database. Content is simple Markdown (# headings, - bullets, 1. numbers, - [ ] to-dos, > quotes, ``` code). For a database row, properties maps column names to plain-text values.",
         {"parent_id": S, "parent_type": {"type": "string", "enum": ["page", "database"]}, "title": S, "content": S,
          "properties": {"type": "object", "additionalProperties": S}}, ["parent_id", "parent_type", "title"]),
    tool("append_to_notion_page", "Queue adding content (simple Markdown) to the end of a Notion page.",
         {"page_id": S, "content": S}, ["page_id", "content"]),
    tool("update_notion_properties", "Queue changing a database row's properties (column name to plain-text value), or a page's title.",
         {"page_id": S, "properties": {"type": "object", "additionalProperties": S}}, ["page_id", "properties"]),
    tool("archive_notion_page", "Queue moving a Notion page or database row to Notion's trash (restorable).", {"page_id": S}, ["page_id"]),
    tool("add_discord_file_to_notion", "Queue adding a file attached to a Discord message to the end of a Notion page (images show inline).",
         {"message_url": S, "page_id": S, "attachment": S, "caption": S}, ["message_url", "page_id"]),
]


def need_drive():
    if not drive.configured():
        raise ValueError("Google Drive isn't connected yet (ieeecsucf@gmail.com hasn't signed in with scripts/drive_login.py)")

def need_notion():
    if not notion.configured():
        raise ValueError("Notion isn't connected (no NOTION_TOKEN)")

def drive_id(value):
    """A Drive id from an id or a link."""
    found = re.search(r"/(?:d|folders)/([\w-]{10,})", value) or re.search(r"[?&]id=([\w-]{10,})", value)
    return found[1] if found else value.strip()

def notion_id(value):
    """A Notion id from an id or a link (links end in the 32-character id, after the title)."""
    value = value.strip()
    if "notion." in value:
        path = urlparse(value).path.replace("-", "")
        found = re.search(r"[0-9a-f]{32}$", path)
        return found[0] if found else value
    return value


class FileTools:
    """Mixed into agent.Ask, which provides bot, guild, asker, channel and queue()."""

    # ---------- Discord attachments ----------

    def find_channel(self, name):
        name = name.strip().lstrip("#").casefold()
        places = list(self.guild.text_channels) + list(self.guild.threads) + list(getattr(self.guild, "voice_channels", []))
        exact = [c for c in places if c.name.casefold() == name]
        loose = [c for c in places if name in c.name.casefold()]
        if not (exact or loose):
            raise ValueError(f"no channel or thread called #{name}")
        return (exact or loose)[0]

    async def t_find_discord_files(self, channel, from_asker=False, limit=10):
        place = self.find_channel(channel)
        rows = []
        async for m in place.history(limit=200):
            if m.attachments and (not from_asker or m.author.id == self.asker.id):
                files = ", ".join(f"[{n}] {a.filename} ({a.content_type or 'file'}, {a.size // 1024} KB)"
                                  for n, a in enumerate(m.attachments, 1))
                rows.append(f"{m.jump_url} | {m.author.display_name} | {m.created_at:%b %-d %H:%M} UTC | {files}"
                            + (f" | “{m.content[:100]}”" if m.content else ""))
                if len(rows) >= int(limit):
                    break
        return "\n".join(rows) or f"No files in #{place.name} in the last 200 messages."

    async def attachment(self, message_url, which=None):
        """(attachment, message) for a message link; which is a file name or a 1-based number."""
        found = re.search(r"channels/\d+/(\d+)/(\d+)", message_url)
        if not found:
            raise ValueError("that isn't a Discord message link")
        channel = self.bot.get_channel(int(found[1])) or await self.bot.fetch_channel(int(found[1]))
        message = await channel.fetch_message(int(found[2]))
        if not message.attachments:
            raise ValueError("that message has no attachments")
        if which and str(which).isdigit() and 1 <= int(which) <= len(message.attachments):
            return message.attachments[int(which) - 1], message
        if which:
            match = [a for a in message.attachments if str(which).casefold() in a.filename.casefold()]
            if not match:
                raise ValueError(f"no attachment named {which}; it has {', '.join(a.filename for a in message.attachments)}")
            return match[0], message
        return message.attachments[0], message

    # ---------- Drive ----------

    async def drive_name(self, file_id):
        f = await asyncio.to_thread(drive.info, file_id)
        return f["name"]

    async def t_search_drive(self, text="", kind="any", folder_id=None):
        need_drive()
        rows = await asyncio.to_thread(drive.search, text, kind, drive_id(folder_id) if folder_id else None)
        return "\n".join(rows) or "Nothing found."

    async def t_read_drive_file(self, file_id):
        need_drive()
        name, link, text = await asyncio.to_thread(drive.read, drive_id(file_id))
        return f"{name} | {link}\n\n{text}"

    async def t_read_sheet(self, sheet_id, range=""):
        need_drive()
        rows = await asyncio.to_thread(drive.sheet_values, drive_id(sheet_id), range)
        return "\n".join(" | ".join(r) for r in rows[:300]) or "That range is empty."

    async def t_save_discord_file_to_drive(self, message_url, folder_id, attachment=None, new_name=None):
        need_drive()
        file, _ = await self.attachment(message_url, attachment)
        folder = drive_id(folder_id)
        folder_name = await self.drive_name(folder)
        name = new_name or file.filename
        async def apply():
            data = await file.read()
            link = await asyncio.to_thread(drive.upload, data, name, file.content_type, folder)
            return f"Saved {name} to {folder_name}: {link}"
        return self.queue(f"save **{file.filename}** to Drive folder **{folder_name}**"
                          + (f" as **{name}**" if name != file.filename else ""), apply)

    async def t_create_drive_folder(self, name, parent_folder_id=None):
        need_drive()
        parent = drive_id(parent_folder_id) if parent_folder_id else None
        where = await self.drive_name(parent) if parent else "My Drive"
        async def apply():
            return f"Created folder {name}: {await asyncio.to_thread(drive.create_folder, name, parent)}"
        return self.queue(f"create Drive folder **{name}** in **{where}**", apply)

    async def t_move_drive_file(self, file_id, folder_id):
        need_drive()
        fid, folder = drive_id(file_id), drive_id(folder_id)
        name, folder_name = await self.drive_name(fid), await self.drive_name(folder)
        async def apply():
            await asyncio.to_thread(drive.move, fid, folder)
            return f"Moved {name} to {folder_name}."
        return self.queue(f"move **{name}** to **{folder_name}**", apply)

    async def t_rename_drive_file(self, file_id, name):
        need_drive()
        fid = drive_id(file_id)
        old = await self.drive_name(fid)
        async def apply():
            await asyncio.to_thread(drive.rename, fid, name)
            return f"Renamed {old} to {name}."
        return self.queue(f"rename **{old}** → **{name}**", apply)

    async def t_copy_drive_file(self, file_id, name=None, folder_id=None):
        need_drive()
        fid, folder = drive_id(file_id), drive_id(folder_id) if folder_id else None
        old = await self.drive_name(fid)
        async def apply():
            return f"Copied {old}: {await asyncio.to_thread(drive.copy, fid, name, folder)}"
        return self.queue(f"copy **{old}**" + (f" as **{name}**" if name else ""), apply)

    async def t_trash_drive_file(self, file_id):
        need_drive()
        fid = drive_id(file_id)
        name = await self.drive_name(fid)
        async def apply():
            await asyncio.to_thread(drive.trash, fid)
            return f"Moved {name} to Drive's trash (restorable for 30 days)."
        return self.queue(f"move **{name}** to Drive's trash", apply)

    async def t_share_drive_file(self, file_id, email, role):
        need_drive()
        fid = drive_id(file_id)
        name = await self.drive_name(fid)
        async def apply():
            await asyncio.to_thread(drive.share, fid, email.strip(), role)
            return f"Shared {name} with {email} ({role}); Google emailed them the link."
        return self.queue(f"share **{name}** with **{email.strip()}** as {role} (Google emails them)", apply)

    async def t_create_google_doc(self, title, text, folder_id=None):
        need_drive()
        folder = drive_id(folder_id) if folder_id else None
        where = await self.drive_name(folder) if folder else "My Drive"
        async def apply():
            return f"Created {title}: {await asyncio.to_thread(drive.create_doc, title, text, folder)}"
        return self.queue(f"create Google Doc **{title}** in **{where}** ({len(text)} characters)", apply)

    async def t_append_to_google_doc(self, doc_id, text):
        need_drive()
        did = drive_id(doc_id)
        name = await self.drive_name(did)
        async def apply():
            await asyncio.to_thread(drive.append_to_doc, did, text)
            return f"Added text to {name}."
        return self.queue(f"add to the end of **{name}**:\n> " + text[:200].replace("\n", "\n> "), apply)

    async def t_append_sheet_rows(self, sheet_id, rows, tab=""):
        need_drive()
        sid = drive_id(sheet_id)
        name = await self.drive_name(sid)
        rows = [[str(c) for c in r] for r in rows]
        async def apply():
            await asyncio.to_thread(drive.append_rows, sid, rows, tab)
            return f"Added {len(rows)} row(s) to {name}."
        preview = "\n".join("> " + " | ".join(r) for r in rows[:5])
        return self.queue(f"add {len(rows)} row(s) to **{name}**{f' ({tab})' if tab else ''}:\n{preview}", apply)

    # ---------- Notion ----------

    async def t_search_notion(self, text="", kind="any"):
        need_notion()
        rows = await notion.search(text, kind)
        return "\n".join(f"{i} | {k} | {t} | {u} | {e[:10]}" for i, k, t, u, e in rows) or "Nothing found."

    async def t_read_notion_page(self, page_id):
        need_notion()
        title, url, props, body = await notion.read_page(notion_id(page_id))
        return f"{title} | {url}\n{props}\n\n{body[:30000]}"

    async def t_query_notion_database(self, database_id, contains=""):
        need_notion()
        title, schema, rows = await notion.query_database(notion_id(database_id), contains)
        cols = ", ".join(f"{n} ({k})" for n, k in schema.items())
        return f"{title}\nColumns: {cols}\n\n" + ("\n".join(rows) or "No rows.")

    async def notion_title(self, page_id):
        title, _, _, _ = await notion.read_page(page_id)
        return title

    async def t_create_notion_page(self, parent_id, parent_type, title, content="", properties=None):
        need_notion()
        pid = notion_id(parent_id)
        async def apply():
            return f"Created {title}: {await notion.create_page(pid, parent_type, title, content, properties)}"
        kind = "row" if parent_type == "database" else "page"
        extra = "".join(f"\n> {k}: {v}" for k, v in (properties or {}).items())
        return self.queue(f"create Notion {kind} **{title}**" + (f" with {len(content)} characters of content" if content else "")
                          + extra, apply)

    async def t_append_to_notion_page(self, page_id, content):
        need_notion()
        pid = notion_id(page_id)
        name = await self.notion_title(pid)
        async def apply():
            await notion.append_text(pid, content)
            return f"Added to {name}."
        return self.queue(f"add to Notion page **{name}**:\n> " + content[:200].replace("\n", "\n> "), apply)

    async def t_update_notion_properties(self, page_id, properties):
        need_notion()
        pid = notion_id(page_id)
        name = await self.notion_title(pid)
        async def apply():
            await notion.update_properties(pid, properties)
            return f"Updated {name}."
        return self.queue(f"update **{name}**: " + ", ".join(f"{k} → {v}" for k, v in properties.items()), apply)

    async def t_archive_notion_page(self, page_id):
        need_notion()
        pid = notion_id(page_id)
        name = await self.notion_title(pid)
        async def apply():
            await notion.archive(pid)
            return f"Moved {name} to Notion's trash."
        return self.queue(f"move Notion page **{name}** to the trash", apply)

    async def t_add_discord_file_to_notion(self, message_url, page_id, attachment=None, caption=""):
        need_notion()
        file, _ = await self.attachment(message_url, attachment)
        pid = notion_id(page_id)
        name = await self.notion_title(pid)
        async def apply():
            await notion.add_file(pid, await file.read(), file.filename, file.content_type or "application/octet-stream", caption)
            return f"Added {file.filename} to {name}."
        return self.queue(f"add **{file.filename}** to Notion page **{name}**", apply)
