"""Downloads job lists, company boards and Workday sites and boils them down to slim listings.

The full responses take well over 100 MB of Python objects to parse, which a
long-running process never hands back to the OS, so job_listings runs this
module as a short-lived child process:

    python -m bot.utils.job_worker < request.json > result.json

Request: {"cutoff": ts, "user_agent": str,
          "title_filter": {include: [regex], exclude: regex, companies: regex, company_titles: regex},
          "student_filter": {include: regex, exclude: regex},
          "lists": [{source, track, url, etag}], "boards": [{ats, slug, company, domain}],
          "workday": [{tenant, wd, site, company, domain}]}
Result:  {"lists": [{url, etag, listings}], "boards": [{slug, listings}], "workday": [{tenant, site, listings}]}
A null "listings" means unchanged (lists only) or failed; the caller keeps what it had.

This file is shared as-is between the IEEE CS and EMBS bots.
"""
import re
import sys
import json
import time
import datetime
import http.client
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor

USER_AGENT = "IEEE-UCF-Bot"  # replaced by the request's user_agent
BOARD_URLS = {
    "greenhouse": "https://boards-api.greenhouse.io/v1/boards/{slug}/jobs",
    "lever": "https://api.lever.co/v0/postings/{slug}?mode=json",
    "ashby": "https://api.ashbyhq.com/posting-api/job-board/{slug}",
}

_SEASON = re.compile(
    r"\b(summer|fall|winter|spring)\b(?:\s*[-/]?\s*(20\d\d))?|\b(20\d\d)\s*[-/]?\s*(summer|fall|winter|spring)\b",
    re.I
)

# Workday career sites: the JSON endpoint behind each company's public job search page.
# Worker types and job families are company-specific, so they're picked out by name.
WORKDAY_PAGE_SIZE = 20
WORKDAY_MAX_PAGES = 15
WORKDAY_STUDENT_TYPES = re.compile(r"intern|co-?op|apprentice|college|graduate|student|early", re.I)
WORKDAY_ENGINEERING_FAMILIES = re.compile(r"engineer|r&d|research|technolog|software|hardware|quality|raqa|regulatory|science|product development|data", re.I)
_WORKDAY_POSTED = re.compile(r"posted (today|yesterday|(\d+)\+? days? ago)", re.I)


# ---------- locations ----------

US_STATES = {
    "AL": "Alabama", "AK": "Alaska", "AZ": "Arizona", "AR": "Arkansas", "CA": "California", "CO": "Colorado",
    "CT": "Connecticut", "DE": "Delaware", "FL": "Florida", "GA": "Georgia", "HI": "Hawaii", "ID": "Idaho",
    "IL": "Illinois", "IN": "Indiana", "IA": "Iowa", "KS": "Kansas", "KY": "Kentucky", "LA": "Louisiana",
    "ME": "Maine", "MD": "Maryland", "MA": "Massachusetts", "MI": "Michigan", "MN": "Minnesota",
    "MS": "Mississippi", "MO": "Missouri", "MT": "Montana", "NE": "Nebraska", "NV": "Nevada",
    "NH": "New Hampshire", "NJ": "New Jersey", "NM": "New Mexico", "NY": "New York", "NC": "North Carolina",
    "ND": "North Dakota", "OH": "Ohio", "OK": "Oklahoma", "OR": "Oregon", "PA": "Pennsylvania",
    "RI": "Rhode Island", "SC": "South Carolina", "SD": "South Dakota", "TN": "Tennessee", "TX": "Texas",
    "UT": "Utah", "VT": "Vermont", "VA": "Virginia", "WA": "Washington", "WV": "West Virginia",
    "WI": "Wisconsin", "WY": "Wyoming", "DC": "District of Columbia", "PR": "Puerto Rico",
}
CA_PROVINCES = {
    "AB": "Alberta", "BC": "British Columbia", "MB": "Manitoba", "NB": "New Brunswick", "NL": "Newfoundland",
    "NS": "Nova Scotia", "ON": "Ontario", "PE": "Prince Edward Island", "QC": "Quebec", "SK": "Saskatchewan",
    "NT": "Northwest Territories", "YT": "Yukon", "NU": "Nunavut",
}
US_CANADA_COUNTRIES = {"United States of America", "United States", "USA", "Canada", "Puerto Rico"}

# Codes are matched case-sensitively and only where a code would sit ("Austin, TX", "US-FL-Largo", "US, CA, ...")
_US_CODE = re.compile(r"(?:,\s*|^US[-, ]+|\bUS-)(?:" + "|".join(US_STATES) + r")\b")
_US_WORDS = re.compile(r"\b(united states|u\.s\.a?\.?|usa|" + "|".join(US_STATES.values()) + r")\b|^us\b", re.I)
_CA_CODE = re.compile(r",\s*(?:" + "|".join(CA_PROVINCES) + r")\b|^CA[-, ]+(?:" + "|".join(CA_PROVINCES) + r")\b")
_CA_WORDS = re.compile(r"\b(canada|" + "|".join(CA_PROVINCES.values()) + r")\b", re.I)
_FOREIGN = re.compile(
    r"\b(united kingdom|uk|england|scotland|wales|northern ireland|ireland|germany|france|netherlands|belgium|"
    r"switzerland|austria|italy|spain|portugal|poland|czech\w*|hungary|romania|sweden|norway|denmark|finland|"
    r"israel|india|china|japan|korea|taiwan|singapore|malaysia|vietnam|thailand|philippines|indonesia|australia|"
    r"new zealand|mexico|brazil|argentina|chile|colombia|costa rica|south africa|egypt|united arab emirates|uae|"
    r"saudi arabia|qatar|turkey|greece|ukraine|serbia|bulgaria|slovakia|lithuania|estonia|latvia|croatia|slovenia|"
    r"london|cambridge, uk|bengaluru|bangalore|hyderabad|pune|chennai|noida|gurgaon|gurugram|mumbai|delhi|"
    r"shanghai|beijing|shenzhen|suzhou|hangzhou|tokyo|osaka|seoul|taipei|hsinchu|hong kong|tel aviv|haifa|"
    r"jerusalem|munich|berlin|dresden|paris|grenoble|amsterdam|eindhoven|dublin|cork|galway|limerick|belfast|"
    r"edinburgh|zurich|geneva|stockholm|copenhagen|helsinki|oslo|warsaw|krakow|kraków|gdansk|wroclaw|bucharest|"
    r"brno|prague|budapest|madrid|barcelona|milan|vienna|sydney|melbourne|brisbane|tijuana|guadalajara|"
    r"mexico city|monterrey|juarez|sao paulo|são paulo|penang|kuala lumpur|manila|ho chi minh|bangkok|dubai)\b",
    re.I
)

def classify_location(text):
    """'us', 'ca', 'foreign', or None when the text doesn't say (e.g. "Remote", "3 Locations", site codes)."""
    text = " ".join((text or "").split())
    if not text:
        return None
    if _US_CODE.search(text) or _US_WORDS.search(text):
        return "us"
    if _CA_CODE.search(text) or _CA_WORDS.search(text):
        return "ca"
    if _FOREIGN.search(text):
        return "foreign"
    return None


# ---------- helpers ----------

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

def http_json(url, body=None, attempts=3):
    """Workday slows down bursts of requests, so timeouts, dropped connections and server errors
    are retried with backoff."""
    headers = {"User-Agent": USER_AGENT, "Accept": "application/json"}
    data = None
    if body is not None:
        headers["Content-Type"] = "application/json"
        data = json.dumps(body).encode()
    for attempt in range(attempts):
        request = urllib.request.Request(url, data=data, headers=headers, method="POST" if data else "GET")
        try:
            with urllib.request.urlopen(request, timeout=30) as resp:
                return json.load(resp)
        except urllib.error.HTTPError as e:
            if e.code not in (429, 500, 502, 503, 504) or attempt == attempts - 1:
                raise
        except (urllib.error.URLError, TimeoutError, ConnectionError, http.client.HTTPException):
            if attempt == attempts - 1:
                raise
        time.sleep(3 * (attempt + 1))

def title_filter(spec):
    """Builds a check that a listing's title matches one of 'include' and not 'exclude', or
    that its company matches 'companies' and its title 'company_titles' (all case-insensitive)."""
    if not spec:
        return lambda title, company: True
    include = [re.compile(rx, re.I) for rx in spec.get('include', [])]
    exclude = re.compile(spec['exclude'], re.I) if spec.get('exclude') else None
    companies = re.compile(spec['companies'], re.I) if spec.get('companies') else None
    company_titles = re.compile(spec.get('company_titles') or '', re.I)
    def keep(title, company):
        title = " ".join(title.split())
        if exclude and exclude.search(title):
            return False
        if not include or any(rx.search(title) for rx in include):
            return True
        return bool(companies and companies.search(" ".join(company.split())) and company_titles.search(title))
    return keep

def student_filter(spec):
    """Boards and Workday list every role, so their titles must also say they're for students."""
    if not spec:
        return lambda title: True
    include = re.compile(spec['include'], re.I)
    exclude = re.compile(spec['exclude'], re.I) if spec.get('exclude') else None
    return lambda title: bool(include.search(title)) and not (exclude and exclude.search(title))

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

def _listing(**fields):
    listing = {
        'track': None, 'logo_fallback': None, 'terms': [], 'sponsorship': None,
        'degrees': [], 'countries': None, 'active': True,
    }
    listing.update(fields)
    return listing


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
        listings.append(_listing(
            id=item['id'],
            source=source,
            track=track,
            title=item.get('title') or '',
            company=item.get('company_name') or '',
            company_url=item.get('company_url'),
            url=item['url'],
            locations=list(item.get('locations') or []),
            terms=[t for t in (terms or []) if t != 'N/A'],
            sponsorship=item.get('sponsorship'),
            degrees=item.get('degrees') or [],
            date_posted=item['date_posted'],
        ))
    return listings

def load_list(spec, cutoff, keep):
    try:
        raw, etag = http_get(spec['url'], spec.get('etag'))
        if raw is None:
            return {'url': spec['url'], 'etag': etag, 'listings': None}
        listings = parse_github_list(json.loads(raw), spec['source'], spec['track'], cutoff)
        return {'url': spec['url'], 'etag': etag, 'listings': [l for l in listings if keep(l['title'], l['company'])]}
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

    return [_listing(
        id=f"{ats}:{slug}:{job_id}",
        source=company,
        title=title,
        company=company,
        company_url=f"https://simplify.jobs/c/{company.replace(' ', '-')}",
        logo_fallback=f"https://www.google.com/s2/favicons?domain={domain}&sz=128",
        url=job_url,
        locations=locations,
        terms=terms_from_title(title),
        date_posted=posted,
    ) for job_id, title, job_url, locations, posted in jobs if posted >= cutoff]

def load_board(spec, cutoff, keep, is_student):
    try:
        raw, _ = http_get(BOARD_URLS[spec['ats']].format(slug=spec['slug']))
        listings = parse_board(json.loads(raw), spec['ats'], spec['slug'], spec['company'], spec['domain'], cutoff)
        listings = [l for l in listings if is_student(l['title']) and keep(l['title'], l['company'])]
        return {'slug': spec['slug'], 'listings': listings}
    except Exception as e:
        print(f"Failed to load {spec['company']} job board: {e}", file=sys.stderr)
        return {'slug': spec['slug'], 'listings': None}


# ---------- Workday ----------

def workday_search(base, facets, page_limit, cutoff_days):
    """Pages through a Workday job search, newest first. Returns (postings, facets of the first page)."""
    postings, first_facets, total = [], [], 0
    for page in range(page_limit):
        data = http_json(f"{base}/jobs", {"appliedFacets": facets, "limit": WORKDAY_PAGE_SIZE,
                                          "offset": page * WORKDAY_PAGE_SIZE, "searchText": ""})
        batch = data.get("jobPostings") or []
        if page == 0:
            total, first_facets = data.get("total") or 0, data.get("facets") or []
        postings += batch
        if not batch or (page + 1) * WORKDAY_PAGE_SIZE >= total:
            break
        # Past the first page (some sites pin older jobs on top), stop once everything is too old
        if page > 0 and all(workday_days_ago(p) > cutoff_days for p in batch):
            break
    return postings, first_facets

def workday_days_ago(posting):
    m = _WORKDAY_POSTED.search(posting.get("postedOn") or "")
    if not m:
        return 10**6
    if m.group(1).lower() == "today":
        return 0
    if m.group(1).lower() == "yesterday":
        return 1
    days = int(m.group(2))
    return days + 1 if "+" in m.group(1) else days

def facet_ids(facets, parameter, pattern):
    for facet in facets:
        if facet.get("facetParameter") == parameter:
            return [v["id"] for v in facet.get("values", []) if "id" in v and pattern.search(v.get("descriptor", ""))]
    return None

def workday_locations(base, posting):
    """Returns (locations, countries) for a posting, asking Workday for the job's country
    only when its location text doesn't make it clear."""
    text = posting.get("locationsText") or ""
    where = classify_location(text)
    if where == "us":
        return [text], ["United States of America"]
    if where == "ca":
        return [text], ["Canada"]
    if where == "foreign":
        return [text], ["(outside US/Canada)"]

    info = http_json(base + posting["externalPath"]).get("jobPostingInfo") or {}
    country = (info.get("country") or {}).get("descriptor")
    short = {"United States of America": "USA"}.get(country, country)
    locations = []
    for l in [info.get("location")] + list(info.get("additionalLocations") or []):
        if l:
            # Site codes like "KNV I" mean nothing to readers, so say which country they're in
            locations.append(f"{l}, {short}" if short and classify_location(l) is None and l == info.get("location") else l)
    countries = [country]
    for extra in info.get("additionalLocations") or []:
        countries.append({"us": "United States of America", "ca": "Canada"}.get(classify_location(extra)))
    return locations or ([text] if text else []), [c for c in countries if c]

def load_workday(spec, cutoff, keep, is_student):
    try:
        today = datetime.datetime.now(datetime.timezone.utc).replace(hour=0, minute=0, second=0, microsecond=0)
        cutoff_days = max(0, int((today.timestamp() - cutoff) // 86400))
        host = f"{spec['tenant']}.{spec['wd']}.myworkdayjobs.com"
        base = f"https://{host}/wday/cxs/{spec['tenant']}/{spec['site']}"

        # One cheap call to learn this site's worker types and job families
        _, facets = workday_search(base, {}, 1, cutoff_days)
        passes = []
        students = facet_ids(facets, "workerSubType", WORKDAY_STUDENT_TYPES)
        if students:
            passes.append({"workerSubType": students})
        engineering = facet_ids(facets, "jobFamilyGroup", WORKDAY_ENGINEERING_FAMILIES)
        passes.append({"jobFamilyGroup": engineering} if engineering else {})

        seen, listings = set(), []
        for applied in passes:
            postings, _ = workday_search(base, applied, WORKDAY_MAX_PAGES, cutoff_days)
            for p in postings:
                path, title, days = p.get("externalPath"), p.get("title") or "", workday_days_ago(p)
                if not path or path in seen or days > cutoff_days:
                    continue
                seen.add(path)
                # Filter before the per-job country lookups, which cost a request each
                if not is_student(title) or not keep(title, spec['company']):
                    continue
                try:
                    locations, countries = workday_locations(base, p)
                except Exception:
                    locations, countries = ([p["locationsText"]] if p.get("locationsText") else []), None
                listings.append(_listing(
                    id=f"workday:{spec['tenant']}:{path}",
                    source=spec['company'],
                    title=title,
                    company=spec['company'],
                    company_url=f"https://simplify.jobs/c/{spec['company'].replace(' ', '-')}",
                    logo_fallback=f"https://www.google.com/s2/favicons?domain={spec['domain']}&sz=128",
                    url=f"https://{host}/{spec['site']}{path}",
                    locations=locations,
                    countries=countries,
                    terms=terms_from_title(title),
                    date_posted=int(today.timestamp()) - days * 86400,
                ))
        return {'tenant': spec['tenant'], 'site': spec['site'], 'listings': listings}
    except Exception as e:
        print(f"Failed to load {spec['company']} Workday site: {e}", file=sys.stderr)
        return {'tenant': spec['tenant'], 'site': spec['site'], 'listings': None}


def main():
    global USER_AGENT
    request = json.load(sys.stdin)
    USER_AGENT = request.get('user_agent') or USER_AGENT
    cutoff = request.get('cutoff', 0)
    keep = title_filter(request.get('title_filter'))
    is_student = student_filter(request.get('student_filter'))
    lists = [load_list(spec, cutoff, keep) for spec in request.get('lists', [])]
    with ThreadPoolExecutor(max_workers=4) as pool:
        boards = list(pool.map(lambda spec: load_board(spec, cutoff, keep, is_student), request.get('boards', [])))
        workday = list(pool.map(lambda spec: load_workday(spec, cutoff, keep, is_student), request.get('workday', [])))
    json.dump({'lists': lists, 'boards': boards, 'workday': workday}, sys.stdout)


if __name__ == '__main__':
    main()
