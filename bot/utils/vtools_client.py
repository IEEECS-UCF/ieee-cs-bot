import datetime
import aiohttp

# vTools Events public API (no sign-in needed). It can't filter by host, so we
# pull one day at a time and keep the events our branch hosts or co-hosts.
API = "https://events.vtools.ieee.org/RST/events/api/public/v8/events/list"
HEADERS = {"User-Agent": "IEEE-CS-UCF-Bot (vtools check)"}
TIMEOUT = aiohttp.ClientTimeout(total=120)


def hosted_by(attrs, spoid):
    hosts = [attrs.get('primary-host') or {}] + list(attrs.get('cohosts') or [])
    return any((h or {}).get('spoid') == spoid for h in hosts)

async def fetch_branch_events(session, day, spoid):
    """Events on the given UTC day window that the branch hosts or co-hosts."""
    start = datetime.datetime.combine(day, datetime.time(), datetime.timezone.utc)
    # Pad the window so evening ET events (next day in UTC) are included
    span = f"{start:%Y-%m-%dT%H:%M:%SZ}~{start + datetime.timedelta(hours=34):%Y-%m-%dT%H:%M:%SZ}"
    results, page = [], 1
    while True:
        params = {"span": span, "limit": "1000", "page": str(page)}
        async with session.get(API, params=params, headers=HEADERS, timeout=TIMEOUT) as resp:
            resp.raise_for_status()
            data = (await resp.json(content_type=None)).get("data", [])
        results.extend(e["attributes"] for e in data if hosted_by(e["attributes"], spoid))
        if len(data) < 1000:
            return results
        page += 1
