"""Downloads job lists and company boards and boils them down to slim listings.

The full responses take well over 100 MB of Python objects to parse, which a
long-running process never hands back to the OS, so job_listings runs this
module as a short-lived child process:

    python -m bot.utils.job_worker < request.json > result.json

Request: {"cutoff": ts, "title_filter": {include: [regex], exclude: regex},
          "lists": [{source, track, url, etag}], "boards": [{ats, slug, company, domain}]}
Result:  {"lists": [{url, etag, listings}], "boards": [{slug, listings}]}
A null "listings" means unchanged (lists only) or failed; the caller keeps what it had.
"""
import re
import sys
import json
import datetime
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor

USER_AGENT = "IEEE-CS-UCF-Bot (+https://github.com/IEEECS-UCF/ieee-cs-bot)"
BOARD_URLS = {
    "greenhouse": "https://boards-api.greenhouse.io/v1/boards/{slug}/jobs",
    "lever": "https://api.lever.co/v0/postings/{slug}?mode=json",
    "ashby": "https://api.ashbyhq.com/posting-api/job-board/{slug}",
}

_SEASON = re.compile(
    r"\b(summer|fall|winter|spring)\b(?:\s*[-/]?\s*(20\d\d))?|\b(20\d\d)\s*[-/]?\s*(summer|fall|winter|spring)\b",
    re.I
)


def http_get(url, etag=None):
    """Returns (body, etag); body is None if unchanged since 'etag'."""
    headers = {"User-Agent": USER_AGENT}
    if etag:
        headers["If-None-Match"] = etag
    try:
        with urllib.request.urlopen(urllib.request.Request(url, headers=headers), timeout=60) as resp:
            return resp.read(), resp.headers.get("ETag")
    except urllib.error.HTTPError as e:
        if e.code == 304:
            return None, etag
        raise

def title_filter(spec):
    """Builds a check that a title matches one of 'include' and not 'exclude' (case-insensitive)."""
    if not spec:
        return lambda title: True
    include = [re.compile(rx, re.I) for rx in spec.get('include', [])]
    exclude = re.compile(spec['exclude'], re.I) if spec.get('exclude') else None
    def keep(title):
        title = " ".join(title.split())
        if exclude and exclude.search(title):
            return False
        return not include or any(rx.search(title) for rx in include)
    return keep

def parse_iso(value):
    try:
        return int(datetime.datetime.fromisoformat(value).timestamp())
    except (TypeError, ValueError):
        return 0

def terms_from_title(title):
    """Pulls cycles like 'Summer 2027' out of a job title."""
    terms = []
    for m in _SEASON.finditer(title):
        season = (m.group(1) or m.group(4)).title()
        year = m.group(2) or m.group(3)
        term = f"{season} {year}" if year else season
        if term not in terms:
            terms.append(term)
    return terms

def _split_locations(text):
    return [part.strip() for part in re.split(r"[;|]", text or "") if part.strip()]


# ---------- GitHub lists ----------

def parse_github_list(data, source, track, cutoff):
    """A listings.json in the SimplifyJobs format."""
    listings = []
    for item in data:
        if not item.get('active') or not item.get('is_visible', True):
            continue
        if (item.get('date_posted') or 0) < cutoff:
            continue
        terms = item.get('terms')
        if terms is None and item.get('season'):
            terms = [s.strip() for s in item['season'].split('/')]
        listings.append({
            'id': item['id'],
            'source': source,
            'track': track,
            'title': item.get('title') or '',
            'company': item.get('company_name') or '',
            'company_url': item.get('company_url'),
            'logo_fallback': None,
            'url': item['url'],
            'locations': list(item.get('locations') or []),
            'terms': [t for t in (terms or []) if t != 'N/A'],
            'sponsorship': item.get('sponsorship'),
            'degrees': item.get('degrees') or [],
            'date_posted': item['date_posted'],
            'active': True,
        })
    return listings

def load_list(spec, cutoff, keep):
    try:
        raw, etag = http_get(spec['url'], spec.get('etag'))
        if raw is None:
            return {'url': spec['url'], 'etag': etag, 'listings': None}
        listings = parse_github_list(json.loads(raw), spec['source'], spec['track'], cutoff)
        return {'url': spec['url'], 'etag': etag, 'listings': [l for l in listings if keep(l['title'])]}
    except Exception as e:
        print(f"Failed to load {spec['source']} {spec['track']} listings: {e}", file=sys.stderr)
        return {'url': spec['url'], 'etag': spec.get('etag'), 'listings': None}


# ---------- company boards ----------

def parse_board(data, ats, slug, company, domain, cutoff):
    """A Greenhouse, Lever or Ashby job board response."""
    jobs = []
    if ats == "greenhouse":
        for j in data.get("jobs", []):
            jobs.append((j["id"], j["title"], j["absolute_url"],
                         _split_locations((j.get("location") or {}).get("name")),
                         parse_iso(j.get("first_published") or j.get("updated_at"))))
    elif ats == "lever":
        for j in data:
            cats = j.get("categories") or {}
            locations = cats.get("allLocations") or _split_locations(cats.get("location"))
            if j.get("workplaceType") == "remote" and "Remote" not in locations:
                locations.append("Remote")
            jobs.append((j["id"], j["text"], j["hostedUrl"], locations, (j.get("createdAt") or 0) // 1000))
    else:
        for j in data.get("jobs", []):
            if not j.get("isListed", True):
                continue
            locations = [j.get("location")] + [s.get("location") for s in j.get("secondaryLocations") or []]
            locations = [l for l in locations if l]
            if j.get("isRemote") and "Remote" not in locations:
                locations.append("Remote")
            jobs.append((j["id"], j["title"], j["jobUrl"], locations, parse_iso(j.get("publishedAt"))))

    return [{
        'id': f"{ats}:{slug}:{job_id}",
        'source': company,
        'track': None,  # inferred from the title
        'title': title,
        'company': company,
        'company_url': f"https://simplify.jobs/c/{company.replace(' ', '-')}",
        'logo_fallback': f"https://www.google.com/s2/favicons?domain={domain}&sz=128",
        'url': job_url,
        'locations': locations,
        'terms': terms_from_title(title),
        'sponsorship': None,
        'degrees': [],
        'date_posted': posted,
        'active': True,
    } for job_id, title, job_url, locations, posted in jobs if posted >= cutoff]

def load_board(spec, cutoff, keep):
    try:
        raw, _ = http_get(BOARD_URLS[spec['ats']].format(slug=spec['slug']))
        listings = parse_board(json.loads(raw), spec['ats'], spec['slug'], spec['company'], spec['domain'], cutoff)
        return {'slug': spec['slug'], 'listings': [l for l in listings if keep(l['title'])]}
    except Exception as e:
        print(f"Failed to load {spec['company']} job board: {e}", file=sys.stderr)
        return {'slug': spec['slug'], 'listings': None}


def main():
    request = json.load(sys.stdin)
    cutoff = request.get('cutoff', 0)
    keep = title_filter(request.get('title_filter'))
    lists = [load_list(spec, cutoff, keep) for spec in request.get('lists', [])]
    with ThreadPoolExecutor(max_workers=6) as pool:
        boards = list(pool.map(lambda spec: load_board(spec, cutoff, keep), request.get('boards', [])))
    json.dump({'lists': lists, 'boards': boards}, sys.stdout)


if __name__ == '__main__':
    main()
