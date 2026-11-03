import os
import asyncio

import aiohttp

# The Notion reader for the knowledge cache: pages shared with the integration
# whose token is NOTION_TOKEN. Returns the pages that changed since the last
# refresh as (id, title, url, last edited, text).

MAX_CHARS = 200_000  # per page
NOTION_API = "https://api.notion.com/v1"
NOTION_VERSION = "2022-06-28"


# ---------- Notion ----------

def notion_configured():
    return bool(os.getenv("NOTION_TOKEN"))

async def notion_pages(seen):
    """Changed Notion pages: [(id, title, url, last edited, text)], and every id still shared."""
    headers = {"Authorization": f"Bearer {os.getenv('NOTION_TOKEN')}", "Notion-Version": NOTION_VERSION}
    async with aiohttp.ClientSession(headers=headers, timeout=aiohttp.ClientTimeout(total=60)) as session:
        pages, cursor = [], None
        while True:
            body = {"filter": {"property": "object", "value": "page"}, "page_size": 100}
            if cursor:
                body["start_cursor"] = cursor
            result = await notion(session, "POST", "/search", body)
            pages += result.get("results", [])
            if not result.get("has_more"):
                break
            cursor = result["next_cursor"]
        changed = []
        for page in pages:
            if seen.get(page["id"]) == page["last_edited_time"]:
                continue
            text = "\n".join(await notion_text(session, page["id"]))
            changed.append((page["id"], page_title(page), page.get("url", ""), page["last_edited_time"], text[:MAX_CHARS]))
        return changed, {p["id"] for p in pages}

async def notion(session, method, path, body=None):
    for attempt in range(4):
        async with session.request(method, NOTION_API + path, json=body) as resp:
            if resp.status == 429:
                await asyncio.sleep(float(resp.headers.get("Retry-After", 1)))
                continue
            resp.raise_for_status()
            await asyncio.sleep(0.35)  # Notion allows about 3 requests a second
            return await resp.json()
    raise RuntimeError("Notion kept rate limiting")

async def notion_text(session, block_id, depth=0):
    """Plain text of a page's blocks, child blocks included (child pages are indexed on their own)."""
    lines, cursor = [], None
    while True:
        result = await notion(session, "GET", f"/blocks/{block_id}/children?page_size=100"
                              + (f"&start_cursor={cursor}" if cursor else ""))
        for block in result.get("results", []):
            kind = block["type"]
            content = block.get(kind, {})
            text = "".join(t.get("plain_text", "") for t in content.get("rich_text", []))
            if kind == "child_page":
                text = f"[page: {content.get('title', '')}]"
            elif kind == "table_row":
                text = " | ".join("".join(t.get("plain_text", "") for t in cell) for cell in content.get("cells", []))
            if text:
                lines.append(("  " * depth) + text)
            if block.get("has_children") and kind != "child_page" and depth < 5:
                lines += await notion_text(session, block["id"], depth + 1)
        if not result.get("has_more"):
            return lines
        cursor = result["next_cursor"]

def page_title(page):
    for prop in page.get("properties", {}).values():
        if prop.get("type") == "title":
            return "".join(t.get("plain_text", "") for t in prop["title"]) or "Untitled"
    return "Untitled"
