import os
import re
import asyncio

import aiohttp

# Notion for /officer ask and the knowledge cache, through the "IEEE CS bot"
# integration (token in NOTION_TOKEN). It sees the pages shared with it in
# Notion and can read, create, edit and archive them.

API = "https://api.notion.com/v1"
VERSION = "2022-06-28"
MAX_CHARS = 200_000   # per page, for the knowledge cache
TEXT_LIMIT = 2000     # Notion's limit per rich-text piece
BLOCKS_PER_CALL = 100


def configured():
    return bool(os.getenv("NOTION_TOKEN"))

def session():
    return aiohttp.ClientSession(headers={"Authorization": f"Bearer {os.getenv('NOTION_TOKEN')}", "Notion-Version": VERSION},
                                 timeout=aiohttp.ClientTimeout(total=120))

async def call(s, method, path, body=None, **kwargs):
    for _ in range(4):
        async with s.request(method, API + path, json=body, **kwargs) as resp:
            if resp.status == 429:
                await asyncio.sleep(float(resp.headers.get("Retry-After", 1)))
                continue
            data = await resp.json(content_type=None)
            if resp.status >= 400:
                raise RuntimeError(f"Notion: {data.get('message', resp.status)}")
            await asyncio.sleep(0.35)  # Notion allows about 3 requests a second
            return data
    raise RuntimeError("Notion kept rate limiting")

def plain(rich):
    return "".join(t.get("plain_text", "") for t in rich or [])

def title_of(obj):
    if obj.get("object") == "database":
        return plain(obj.get("title")) or "Untitled"
    for prop in obj.get("properties", {}).values():
        if prop.get("type") == "title":
            return plain(prop["title"]) or "Untitled"
    return "Untitled"


# ---------- reading ----------

async def search(query="", kind=None):
    """[(id, object, title, url, last edited)] for pages/databases matching the text."""
    async with session() as s:
        body = {"query": query, "page_size": 25}
        if kind in ("page", "database"):
            body["filter"] = {"property": "object", "value": kind}
        result = await call(s, "POST", "/search", body)
    return [(r["id"], r["object"], title_of(r), r.get("url", ""), r.get("last_edited_time", ""))
            for r in result.get("results", [])]

async def block_lines(s, block_id, depth=0):
    """Plain text of a page's blocks, child blocks included (child pages are their own pages)."""
    lines, cursor = [], None
    while True:
        result = await call(s, "GET", f"/blocks/{block_id}/children?page_size=100"
                            + (f"&start_cursor={cursor}" if cursor else ""))
        for block in result.get("results", []):
            kind = block["type"]
            content = block.get(kind, {})
            text = plain(content.get("rich_text"))
            if kind == "child_page":
                text = f"[page: {content.get('title', '')}] (id {block['id']})"
            elif kind == "child_database":
                text = f"[database: {content.get('title', '')}] (id {block['id']})"
            elif kind == "to_do":
                text = ("[x] " if content.get("checked") else "[ ] ") + text
            elif kind == "table_row":
                text = " | ".join(plain(cell) for cell in content.get("cells", []))
            elif kind in ("image", "file", "pdf"):
                text = f"[{kind}: {plain(content.get('caption')) or content.get('name', '')}]"
            if text:
                lines.append(("  " * depth) + text)
            if block.get("has_children") and kind not in ("child_page", "child_database") and depth < 5:
                lines += await block_lines(s, block["id"], depth + 1)
        if not result.get("has_more"):
            return lines
        cursor = result["next_cursor"]

async def read_page(page_id):
    """(title, url, properties as text, body text)"""
    async with session() as s:
        page = await call(s, "GET", f"/pages/{page_id}")
        lines = await block_lines(s, page_id)
    props = "\n".join(f"{name}: {property_text(p)}" for name, p in page.get("properties", {}).items())
    return title_of(page), page.get("url", ""), props, "\n".join(lines)

async def database_schema(s, database_id):
    db = await call(s, "GET", f"/databases/{database_id}")
    return {name: p["type"] for name, p in db.get("properties", {}).items()}, title_of(db)

async def query_database(database_id, filter_text=""):
    """Rows as text lines, optionally only rows whose text contains filter_text."""
    async with session() as s:
        schema, title = await database_schema(s, database_id)
        rows, cursor = [], None
        while len(rows) < 200:
            body = {"page_size": 100}
            if cursor:
                body["start_cursor"] = cursor
            result = await call(s, "POST", f"/databases/{database_id}/query", body)
            for page in result.get("results", []):
                line = f"{page['id']} | " + " | ".join(f"{name}: {property_text(p)}" for name, p in page["properties"].items())
                if filter_text.casefold() in line.casefold():
                    rows.append(line)
            if not result.get("has_more"):
                break
            cursor = result["next_cursor"]
    return title, schema, rows

def property_text(p):
    kind = p.get("type")
    value = p.get(kind)
    if kind in ("title", "rich_text"):
        return plain(value)
    if kind in ("select", "status"):
        return (value or {}).get("name", "")
    if kind == "multi_select":
        return ", ".join(v["name"] for v in value or [])
    if kind == "date":
        return ((value or {}).get("start") or "") + (f" to {value['end']}" if value and value.get("end") else "")
    if kind in ("number", "checkbox", "url", "email", "phone_number"):
        return "" if value is None else str(value)
    if kind == "people":
        return ", ".join(v.get("name", v.get("id", "")) for v in value or [])
    if kind in ("created_time", "last_edited_time"):
        return value or ""
    return ""

async def changed_pages(seen):
    """Changed pages for the knowledge cache: [(id, title, url, last edited, text)], and every id still shared."""
    async with session() as s:
        pages, cursor = [], None
        while True:
            body = {"filter": {"property": "object", "value": "page"}, "page_size": 100}
            if cursor:
                body["start_cursor"] = cursor
            result = await call(s, "POST", "/search", body)
            pages += result.get("results", [])
            if not result.get("has_more"):
                break
            cursor = result["next_cursor"]
        changed = []
        for page in pages:
            if seen.get(page["id"]) == page["last_edited_time"]:
                continue
            props = "\n".join(f"{n}: {property_text(p)}" for n, p in page.get("properties", {}).items()
                              if p.get("type") != "title" and property_text(p))
            text = "\n".join(([props] if props else []) + await block_lines(s, page["id"]))
            changed.append((page["id"], title_of(page), page.get("url", ""), page["last_edited_time"], text[:MAX_CHARS]))
        return changed, {p["id"] for p in pages}


# ---------- writing ----------

def rich(text):
    return [{"type": "text", "text": {"content": text[i:i + TEXT_LIMIT]}} for i in range(0, len(text), TEXT_LIMIT)] or []

def blocks_from_text(text):
    """Simple Markdown to Notion blocks: headings, bullets, numbers, to-dos, quotes, dividers, code, paragraphs."""
    blocks, code = [], None
    for line in text.splitlines():
        if line.strip().startswith("```"):
            if code is None:
                code = []
            else:
                blocks.append({"type": "code", "code": {"rich_text": rich("\n".join(code)), "language": "plain text"}})
                code = None
            continue
        if code is not None:
            code.append(line)
            continue
        s = line.strip()
        if not s:
            continue
        if s in ("---", "***"):
            blocks.append({"type": "divider", "divider": {}})
        elif m := re.match(r"(#{1,3})\s+(.*)", s):
            kind = f"heading_{len(m[1])}"
            blocks.append({"type": kind, kind: {"rich_text": rich(m[2])}})
        elif m := re.match(r"[-*]\s+\[( |x|X)\]\s+(.*)", s):
            blocks.append({"type": "to_do", "to_do": {"rich_text": rich(m[2]), "checked": m[1] in "xX"}})
        elif m := re.match(r"[-*•]\s+(.*)", s):
            blocks.append({"type": "bulleted_list_item", "bulleted_list_item": {"rich_text": rich(m[1])}})
        elif m := re.match(r"\d+[.)]\s+(.*)", s):
            blocks.append({"type": "numbered_list_item", "numbered_list_item": {"rich_text": rich(m[1])}})
        elif s.startswith(">"):
            blocks.append({"type": "quote", "quote": {"rich_text": rich(s.lstrip("> "))}})
        else:
            blocks.append({"type": "paragraph", "paragraph": {"rich_text": rich(s)}})
    if code is not None:
        blocks.append({"type": "code", "code": {"rich_text": rich("\n".join(code)), "language": "plain text"}})
    return blocks

def property_value(kind, value):
    """A Notion property value from plain text, for the property's type."""
    value = str(value).strip()
    if kind == "title":
        return {"title": rich(value)}
    if kind == "rich_text":
        return {"rich_text": rich(value)}
    if kind == "number":
        return {"number": float(value) if value else None}
    if kind in ("select", "status"):
        return {kind: {"name": value} if value else None}
    if kind == "multi_select":
        return {"multi_select": [{"name": v.strip()} for v in value.split(",") if v.strip()]}
    if kind == "date":
        start, _, end = value.partition(" to ")
        return {"date": {"start": start.strip(), **({"end": end.strip()} if end.strip() else {})} if start.strip() else None}
    if kind == "checkbox":
        return {"checkbox": value.casefold() in ("true", "yes", "y", "1", "x", "done", "checked")}
    if kind in ("url", "email", "phone_number"):
        return {kind: value or None}
    raise ValueError(f"can't set a {kind} property from text")

async def append_blocks(s, block_id, blocks):
    for i in range(0, len(blocks), BLOCKS_PER_CALL):
        await call(s, "PATCH", f"/blocks/{block_id}/children", {"children": blocks[i:i + BLOCKS_PER_CALL]})

async def properties_for(s, database_id, values):
    schema, _ = await database_schema(s, database_id)
    out = {}
    for name, value in values.items():
        if name not in schema:
            raise ValueError(f"no property named {name!r}; it has {', '.join(schema)}")
        out[name] = property_value(schema[name], value)
    return out

async def create_page(parent_id, parent_kind, title, text="", properties=None):
    """New page under a page or as a row in a database; returns its link."""
    async with session() as s:
        if parent_kind == "database":
            schema, _ = await database_schema(s, parent_id)
            title_prop = next(n for n, k in schema.items() if k == "title")
            props = await properties_for(s, parent_id, properties or {})
            props[title_prop] = property_value("title", title)
            body = {"parent": {"database_id": parent_id}, "properties": props}
        else:
            body = {"parent": {"page_id": parent_id}, "properties": {"title": property_value("title", title)}}
        blocks = blocks_from_text(text)
        body["children"] = blocks[:BLOCKS_PER_CALL]
        page = await call(s, "POST", "/pages", body)
        await append_blocks(s, page["id"], blocks[BLOCKS_PER_CALL:])
    return page.get("url", "")

async def append_text(page_id, text):
    async with session() as s:
        await append_blocks(s, page_id, blocks_from_text(text))

async def update_properties(page_id, values):
    async with session() as s:
        page = await call(s, "GET", f"/pages/{page_id}")
        parent = page.get("parent", {})
        if parent.get("type") == "database_id":
            props = await properties_for(s, parent["database_id"], values)
        elif set(values) <= {"title"}:
            props = {"title": property_value("title", values["title"])}
        else:
            raise ValueError("only database rows have properties besides the title")
        await call(s, "PATCH", f"/pages/{page_id}", {"properties": props})

async def archive(page_id):
    """Moves the page to Notion's trash (restorable from there)."""
    async with session() as s:
        await call(s, "PATCH", f"/pages/{page_id}", {"archived": True})

async def add_file(page_id, data, filename, content_type, caption=""):
    """Upload a file to Notion and add it to the page (as an image if it is one)."""
    async with session() as s:
        upload = await call(s, "POST", "/file_uploads", {"filename": filename, "content_type": content_type})
        form = aiohttp.FormData()
        form.add_field("file", data, filename=filename, content_type=content_type)
        async with s.post(f"{API}/file_uploads/{upload['id']}/send", data=form) as resp:
            if resp.status >= 400:
                raise RuntimeError(f"Notion upload failed ({resp.status})")
        kind = "image" if content_type.startswith("image/") else "file"
        block = {"type": kind, kind: {"type": "file_upload", "file_upload": {"id": upload["id"]},
                                      **({"caption": rich(caption)} if caption else {})}}
        await append_blocks(s, page_id, [block])
