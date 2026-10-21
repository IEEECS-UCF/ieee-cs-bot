import re
import html
import json
import asyncio
from urllib.parse import urlparse, parse_qs

import aiohttp

# Community-maintained lists, updated several times a day
SOURCES = {
    "Internship": "https://raw.githubusercontent.com/SimplifyJobs/Summer2027-Internships/dev/.github/scripts/listings.json",
    "New Grad": "https://raw.githubusercontent.com/SimplifyJobs/New-Grad-Positions/dev/.github/scripts/listings.json",
}

USER_AGENT = "IEEE-CS-UCF-Bot (+https://github.com/IEEECS-UCF/ieee-cs-bot)"

_META_TAG = re.compile(r'<meta\s[^>]*>', re.I)
_ATTR = re.compile(r'(\w[\w:-]*)="([^"]*)"')


def new_session():
    return aiohttp.ClientSession(
        headers={"User-Agent": USER_AGENT},
        timeout=aiohttp.ClientTimeout(total=60),
    )

async def fetch_listings(session, url, etag=None):
    """Downloads a SimplifyJobs listings.json.

    Returns (listings, etag); listings is None if the file is unchanged since 'etag'.
    """
    headers = {"If-None-Match": etag} if etag else {}
    async with session.get(url, headers=headers) as resp:
        if resp.status == 304:
            return None, etag
        resp.raise_for_status()
        raw = await resp.read()
        listings = await asyncio.to_thread(json.loads, raw)
        return listings, resp.headers.get("ETag")

async def fetch_company_logo(session, company_url):
    """Finds a company's logo through the og:image on its Simplify page. Returns None if unavailable."""
    if not company_url:
        return None
    try:
        async with session.get(company_url) as resp:
            if resp.status != 200:
                return None
            page = await resp.text()
    except (aiohttp.ClientError, asyncio.TimeoutError):
        return None

    for tag in _META_TAG.findall(page):
        attrs = dict(_ATTR.findall(tag))
        if attrs.get("property") == "og:image" and attrs.get("content"):
            og_image = html.unescape(attrs["content"])
            logo = parse_qs(urlparse(og_image).query).get("logo", [None])[0]
            return logo if logo and logo.startswith("https://") else None
    return None
