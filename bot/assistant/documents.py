import os
import asyncio

import aiohttp
from google.oauth2 import service_account
from googleapiclient.discovery import build
from googleapiclient.errors import HttpError

from bot.utils.gcal_client import CREDENTIALS_FILE

# Google Drive and Notion readers for the knowledge cache. Each returns the
# documents that changed since the last refresh as (id, title, url, modified, text).
#   Drive: whatever is shared with the bot's service account (needs the Drive API
#          turned on in its Cloud project).
#   Notion: pages shared with the integration whose token is NOTION_TOKEN.

MAX_CHARS = 200_000  # per document
DRIVE_EXPORT = {
    "application/vnd.google-apps.document": "text/plain",
    "application/vnd.google-apps.presentation": "text/plain",
    "application/vnd.google-apps.spreadsheet": "text/csv",
}
DRIVE_PLAIN = ("text/plain", "text/markdown", "text/csv")
NOTION_API = "https://api.notion.com/v1"
NOTION_VERSION = "2022-06-28"


# ---------- Google Drive ----------

def drive_service():
    creds = service_account.Credentials.from_service_account_file(
        CREDENTIALS_FILE, scopes=["https://www.googleapis.com/auth/drive.readonly"])
    return build("drive", "v3", credentials=creds, cache_discovery=False)

def drive_files(seen):
    """Changed Drive files: [(id, title, url, modified, text)], and every id still there.
    seen: {file id: modifiedTime} from the last refresh. Runs in a worker thread."""
    service = drive_service()
    files, token = [], None
    while True:
        page = service.files().list(q="trashed = false", pageSize=200, pageToken=token,
                                    fields="nextPageToken, files(id, name, mimeType, modifiedTime, webViewLink)",
                                    includeItemsFromAllDrives=True, supportsAllDrives=True).execute(num_retries=3)
        files += page.get("files", [])
        token = page.get("nextPageToken")
        if not token:
            break
    changed = []
    for f in files:
        if seen.get(f["id"]) == f["modifiedTime"]:
            continue
        try:
            if f["mimeType"] in DRIVE_EXPORT:
                data = service.files().export(fileId=f["id"], mimeType=DRIVE_EXPORT[f["mimeType"]]).execute(num_retries=3)
            elif f["mimeType"] in DRIVE_PLAIN:
                data = service.files().get_media(fileId=f["id"]).execute(num_retries=3)
            else:
                continue  # PDFs, images and the like: only searchable by name, which isn't worth an entry
        except HttpError as e:
            print(f"Knowledge: couldn't read Drive file {f['name']}: {e.status_code}")
            continue
        text = data.decode("utf-8", errors="replace") if isinstance(data, bytes) else str(data)
        changed.append((f["id"], f["name"], f.get("webViewLink", ""), f["modifiedTime"], text[:MAX_CHARS]))
    return changed, {f["id"] for f in files}


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
