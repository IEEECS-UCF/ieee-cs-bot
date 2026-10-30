import aiohttp

# KnightConnect (Campus Labs Engage) public discovery API. It's read-only:
# student orgs can't create or edit events through it, so the bot only
# compares listings against the calendar.
SEARCH = "https://knightconnect.campuslabs.com/engage/api/discovery/event/search"
EVENT_PAGE = "https://knightconnect.campuslabs.com/engage/event/{}"
TIMEOUT = aiohttp.ClientTimeout(total=60)


async def fetch_org_events(session, org_id, take=100):
    """Approved events for the org, newest first, as dicts with a 'link'."""
    params = {
        "organizationIds": str(org_id), "orderByField": "endsOn", "orderByDirection": "descending",
        "status": "Approved", "take": str(take), "query": "",
    }
    async with session.get(SEARCH, params=params, timeout=TIMEOUT) as resp:
        resp.raise_for_status()
        events = (await resp.json(content_type=None)).get("value", [])
    for e in events:
        e["link"] = EVENT_PAGE.format(e["id"])
    return events

async def fetch_event(session, event_id):
    """One listing by ID (for listings an officer linked by hand), or None."""
    async with session.get(f"https://knightconnect.campuslabs.com/engage/api/discovery/event/{event_id}",
                           timeout=TIMEOUT) as resp:
        if resp.status == 404:
            return None
        resp.raise_for_status()
        e = await resp.json(content_type=None)
    e["link"] = EVENT_PAGE.format(e["id"])
    # Single listings keep the room under address.name; search results use 'location'
    e.setdefault("location", None)
    e["location"] = e["location"] or (e.get("address") or {}).get("name")
    return e

def parse_event_id(text):
    """Accept a bare ID or a link like knightconnect.campuslabs.com/engage/event/12931108."""
    digits = "".join(ch if ch.isdigit() else " " for ch in (text or "")).split()
    return digits[-1] if digits else None
