import os
import re
import json
import time
import asyncio
import datetime
import traceback
from urllib.parse import urlparse, parse_qsl, urlencode, urlunparse

import discord
from discord.ext import commands, tasks
from bot.utils.job_listings import (
    GITHUB_LISTS, USER_AGENT, COMPANY_BOARDS, WORKDAY_SITES, new_session, run_worker, fetch_company_logo,
)
from bot.utils.job_worker import classify_location, US_CANADA_COUNTRIES
from bot.utils.formatting import create_base_embed, IEEE_BLUE

ORG_NAME = "IEEE CS"
STATE_FILE = "data/jobs_state.json"
CHECK_MINUTES = 30
BOARD_CHECK_MINUTES = 120     # company boards change slowly; be gentle with them
WORKDAY_CHECK_MINUTES = 360   # Workday sites take many requests each
MAX_AGE_DAYS = 30             # only roles posted within this window are announced
POST_DELAY_SECONDS = 1        # Discord allows about one message per second per channel
REMEMBER_DAYS = 60            # how long to remember roles after they close
WATCH_DAYS = 120              # how long posts are checked for their role closing
CLOSED_PREFIX = "🔒 Closed: "
CLOSED_TEXT = "🔒 No longer accepting applications."
CLOSED_COLOR = 0x747F8D
STATE_VERSION = 2

# Mirrors the Fields of Interest in /info. A listing is posted only if its
# title matches at least one of these.
TOPICS = {
    "Systems Programming": r"systems? (software|programm)|systems development engineer|low[- ]level|low[- ]latency|\bc\+\+|\brust\b|compilers?\b|\bruntime|distributed systems",
    "Comp Arch": r"computer architect|micro-?architect|\bcpu\b|\bsoc\b|\basic\b|\brtl\b|design verification|(pre|post)[- ]silicon|silicon (design|validation|engineer)|physical design|\bchips?\b|(processor|cpu) (design|architect|verification|validation|performance)",
    "GPU Programming": r"\bgpus?\b|\bcuda\b|graphics (software|engineer|programm|driver)|\bvulkan\b|\bopencl\b|\bshaders?\b|\brocm\b|rendering engineer",
    "Embedded Systems": r"\bembedded\b|\bfirmware\b|microcontroller|\brtos\b|real[- ]time (software|systems|embedded)|\bfpgas?\b",
    "OS": r"operating systems?|\bkernel\b|\blinux\b|hypervisor|virtualization|\bbsp\b|bootloader",
    "Drivers": r"(device|kernel|graphics|display|gpu|linux|windows|audio|camera|storage|network|software)[- ]drivers?\b|\bdrivers? (and|engineer|developer|development|software)",
    "Computation": r"\bhpc\b|high[- ]performance comput|parallel (comput|programm)|scientific comput|quantum comput|numerical",
    "Discrete Mathematics": r"cryptograph|formal (verification|methods)|algorithms? (engineer|research|developer|intern)|theoretical computer science|combinatori",
}
EXCLUDE = r"\b(sales|marketing|recruit\w*|mba|accounting|accountant|finance|legal|business|designer|human resources|supply chain|procurement|technician|buyer|quant\w*|trading)\b"

# Company boards list every role, so their titles must also say who it's for
INTERN = r"\b(intern(ship)?s?|co-?op|student)\b"
# Big employers title new grad roles like "Engineer I", "Associate Firmware Engineer" or "... Development Program"
NEW_GRAD = r"\b(new grad(uate)?s?|graduate|university|early[- ]career|entry[- ]level|campus|apprentice)\b|\b20\d\d grads?\b|\b(engineer|developer|scientist)\s+(i|1)\b|\bassociate\b.*\b(engineer|scientist|developer)\b|\b(development|rotation(al)?|leadership) program\b"
SENIOR = r"\b(senior|sr\.?|staff|principal|lead|manager|director|head of)\b"

TOPIC_PATTERNS = {name: re.compile(rx, re.I) for name, rx in TOPICS.items()}
EXCLUDE_PATTERN = re.compile(EXCLUDE, re.I)
INTERN_PATTERN = re.compile(INTERN, re.I)
NEW_GRAD_PATTERN = re.compile(NEW_GRAD, re.I)
SENIOR_PATTERN = re.compile(SENIOR, re.I)

SEASONS = {"Summer": "☀️", "Fall": "🍂", "Winter": "❄️", "Spring": "🌱"}
SPONSORSHIP = {
    "Offers Sponsorship": "🛂 Offers visa sponsorship",
    "Does Not Offer Sponsorship": "🚫 No visa sponsorship",
    "U.S. Citizenship is Required": "🇺🇸 U.S. citizenship required",
}

# Ignored when comparing names, so "Astera Labs Inc." and "Astera Labs" match
COMPANY_FILLER = {"inc", "llc", "ltd", "corp", "corporation", "co", "company", "the", "technologies", "technology", "group", "holdings"}
TITLE_FILLER = {"the", "and", "of", "for", "a", "an", "in", "to", "with", "summer", "fall", "winter", "spring"}
TITLE_SYNONYMS = {
    "engineering": "engineer", "internship": "intern", "interns": "intern", "developer": "engineer",
    "development": "engineer", "i": "1", "ii": "2", "iii": "3",
}
TRACKING_PARAMS = ("utm_", "ref", "source", "gh_src", "lever-source")


def normalize(text):
    return " ".join((text or "").split())

def match_topics(title):
    if EXCLUDE_PATTERN.search(title):
        return []
    return [name for name, rx in TOPIC_PATTERNS.items() if rx.search(title)]

def infer_track(title):
    if SENIOR_PATTERN.search(title):
        return None
    if INTERN_PATTERN.search(title):
        return "Internship"
    if NEW_GRAD_PATTERN.search(title):
        return "New Grad"
    return None

def dedupe_key(company, title):
    """Same company and (roughly) the same title counts as one role, whatever the location or source."""
    company_words = [w for w in re.findall(r"[a-z0-9]+", company.lower()) if w not in COMPANY_FILLER]
    title_words = {
        TITLE_SYNONYMS.get(w, w) for w in re.findall(r"[a-z0-9+#]+", title.lower())
        if w not in TITLE_FILLER and not re.fullmatch(r"20\d\d", w)
    }
    return f"{' '.join(company_words)}|{' '.join(sorted(title_words))}"

def same_role(key_a, key_b):
    """Dedupe keys for the same title where one company name starts the other ("Cboe" / "Cboe Global Markets")."""
    company_a, _, title_a = key_a.partition("|")
    company_b, _, title_b = key_b.partition("|")
    if title_a != title_b:
        return False
    words_a, words_b = company_a.split(), company_b.split()
    shorter, longer = sorted((words_a, words_b), key=len)
    return bool(shorter) and longer[:len(shorter)] == shorter

def url_key(url):
    """The application link without tracking parameters, which differ between sources."""
    parts = urlparse(url.strip())
    query = [(k, v) for k, v in parse_qsl(parts.query) if not k.lower().startswith(TRACKING_PARAMS)]
    path = re.sub(r"/(apply|application)/?$", "", parts.path).rstrip("/")
    if parts.netloc.lower().endswith(".myworkdayjobs.com"):
        # Workday links may carry a locale and vary in case between sources
        path = re.sub(r"^/[a-z]{2}-[a-z]{2}(?=/)", "", path.lower())
    return urlunparse(("https", parts.netloc.lower(), path, "", urlencode(query), ""))

def us_canada_locations(item):
    """The listing's locations minus any outside the US and Canada, or None if it's only elsewhere.
    Locations that don't say where they are ("Remote", "SF") are given the benefit of the doubt."""
    kinds = [classify_location(l) for l in item['locations']]
    if item.get('countries'):
        if not any(c in US_CANADA_COUNTRIES for c in item['countries']):
            return None
    elif kinds and all(k == "foreign" for k in kinds):
        return None
    return [l for l, k in zip(item['locations'], kinds) if k != "foreign"]

def select_jobs(listings, cutoff):
    """Open, on-topic listings posted since 'cutoff', with duplicates across locations and sources merged."""
    roles = {}
    by_url = {}
    by_title = {}
    for item in listings:
        if not item['active'] or item['date_posted'] < cutoff:
            continue
        title = normalize(item['title'])
        topics = match_topics(title)
        if not topics or SENIOR_PATTERN.search(title):
            continue
        track = item['track'] or infer_track(title)
        if track is None:
            continue
        locations = us_canada_locations(item)
        if locations is None:
            continue

        key = dedupe_key(item['company'], title)
        link = url_key(item['url'])
        title_part = key.partition("|")[2]
        role = roles.get(key) or roles.get(by_url.get(link))
        if role is None:
            similar = next((k for k in by_title.get(title_part, []) if same_role(k, key)), None)
            role = roles.get(similar)
        if role is None:
            role = roles[key] = {
                'keys': [key],
                'ids': [item['id']],
                'urls': [link],
                'raw_urls': [item['url']],
                'source': item['source'],
                'track': track,
                'topics': topics,
                'title': title,
                'company': normalize(item['company']),
                'company_url': item['company_url'],
                'logo_fallback': item['logo_fallback'],
                'url': item['url'],
                'locations': locations,
                'terms': list(item['terms']),
                'sponsorship': item['sponsorship'],
                'degrees': item['degrees'],
                'date_posted': item['date_posted'],
            }
        else:
            # Earlier sources (the curated lists) have richer data, so only fill in gaps
            if key not in role['keys']:
                role['keys'].append(key)
                roles[key] = role
            role['ids'].append(item['id'])
            if link not in role['urls']:
                role['urls'].append(link)
            if item['url'] not in role['raw_urls']:
                role['raw_urls'].append(item['url'])
            role['locations'] += [l for l in locations if l not in role['locations']]
            role['terms'] += [t for t in item['terms'] if t not in role['terms']]
            role['logo_fallback'] = role['logo_fallback'] or item['logo_fallback']
            role['date_posted'] = min(role['date_posted'], item['date_posted'])
        by_url[link] = role['keys'][0]
        by_title.setdefault(title_part, []).append(key)

    unique = {id(r): r for r in roles.values()}
    return list(unique.values())

def merge_into_post(post, role):
    for field, values in (("ids", role['ids']), ("keys", role['keys']), ("raw_urls", role['raw_urls'])):
        post[field] += [v for v in values if v not in post[field]]

def is_workday_link(url):
    return ".myworkdayjobs.com/" in url

def format_locations(locations):
    if not locations:
        return "Not stated"
    shown = ", ".join(locations[:3]) if len(locations) <= 3 else "\n".join(locations[:3])
    if len(locations) > 3:
        shown += f"\n+{len(locations) - 3} more"
    return shown[:1024]

def build_highlights(role):
    highlights = []
    seasons = []
    for term in role['terms']:
        season = term.split()[0]
        if season in SEASONS and season not in seasons:
            seasons.append(season)
    highlights += [f"{SEASONS[s]} {s}" for s in seasons]
    if any(l.endswith(", FL") or l.endswith(", Florida") for l in role['locations']):
        highlights.append("🌴 Florida")
    if any("remote" in l.casefold() for l in role['locations']):
        highlights.append("🌐 Remote")
    if role['sponsorship'] in SPONSORSHIP:
        highlights.append(SPONSORSHIP[role['sponsorship']])
    if role['degrees'] and "Bachelor's" not in role['degrees']:
        highlights.append(f"🎓 {'/'.join(role['degrees'])} only")
    return highlights

def build_embed(role, logo_url):
    embed = create_base_embed(
        title=role['title'][:256],
        description=f"[Apply here]({role['url']})",
        color=IEEE_BLUE
    )
    embed.url = role['url']
    # Some sources only record the posting date, so a timestamp could show a misleading time
    embed.timestamp = None
    embed.add_field(name="Company", value=role['company'][:1024], inline=True)
    embed.add_field(name="Location", value=format_locations(role['locations']), inline=True)
    embed.add_field(name="Track", value=role['track'], inline=True)
    if role['track'] == "Internship":
        embed.add_field(name="Cycle", value=", ".join(role['terms']) or "Not stated", inline=True)
    embed.add_field(name="Topic", value=", ".join(role['topics']), inline=True)

    highlights = build_highlights(role)
    if highlights:
        embed.add_field(name="Highlights", value="\n".join(highlights), inline=False)
    if logo_url:
        embed.set_thumbnail(url=logo_url)
    posted = datetime.datetime.fromtimestamp(role['date_posted'], datetime.timezone.utc).date()
    via = "company careers page" if role['source'] == role['company'] else role['source']
    embed.set_footer(text=f"{ORG_NAME} Job Feed • via {via} • Posted {posted.isoformat()}")
    return embed


class JobFeed(commands.Cog):
    """Posts new internship and new grad listings that match the club's fields of interest."""

    def __init__(self, bot):
        self.bot = bot
        self.channel_id = int(os.getenv('JOBS_CHANNEL_ID'))
        self.session = None
        self.list_cache = {}    # url -> (etag, recent listings)
        self.board_cache = {}   # slug -> recent listings
        self.workday_cache = {}  # (tenant, site) -> recent listings
        self.boards_checked = 0
        self.workday_checked = 0
        self.needs_history_sync = False
        self.status_cache = {}  # listing id or job link -> "open" / "closed", as last reported
        self._lock = asyncio.Lock()

        os.makedirs(os.path.dirname(STATE_FILE), exist_ok=True)
        self.state = self.load_state()

    async def cog_load(self):
        self.session = new_session()
        self.check_jobs.start()

    async def cog_unload(self):
        self.check_jobs.cancel()
        if self.session:
            await self.session.close()

    # ---------- state ----------

    def load_state(self):
        state = {}
        # Without a trustworthy state file, the channel itself is checked for jobs already posted
        self.needs_history_sync = True
        if os.path.exists(STATE_FILE):
            try:
                with open(STATE_FILE, "r") as f:
                    state = json.load(f)
                self.needs_history_sync = False
            except (json.JSONDecodeError, OSError) as e:
                print(f"Job state file unreadable ({e}); moving it aside and starting fresh.")
                os.replace(STATE_FILE, STATE_FILE + ".corrupt")
                state = {}
        if state.get("version", 1) < 2:
            # v1 marked a first-run backlog as handled without posting it; the
            # channel history scan works out what was really posted
            state = {"logos": state.get("logos", {}), "legacy": state}
            self.needs_history_sync = True
        state["version"] = STATE_VERSION
        state.setdefault("posted_ids", {})
        state.setdefault("posted_keys", {})
        state.setdefault("posted_urls", {})
        state.setdefault("logos", {})
        # Posts by dedupe key: where each one is, and the ids and links used to tell when its role closes
        state.setdefault("posts", {})
        state.setdefault("posts_synced", False)
        return state

    def save_state(self):
        tmp = STATE_FILE + ".tmp"
        with open(tmp, "w") as f:
            json.dump(self.state, f, indent=2)
        os.replace(tmp, STATE_FILE)

    def mark_posted(self, role, now_ts):
        for table, values in (("posted_ids", role['ids']), ("posted_keys", role['keys']), ("posted_urls", role['urls'])):
            for value in values:
                self.state[table][value] = now_ts

    def posted_by_title(self):
        """Index of posted dedupe keys by their title part, for near-match lookups."""
        index = {}
        for key in self.state["posted_keys"]:
            index.setdefault(key.partition("|")[2], []).append(key)
        return index

    def already_posted(self, role, by_title):
        if (any(k in self.state["posted_keys"] for k in role['keys'])
                or any(u in self.state["posted_urls"] for u in role['urls'])
                or any(i in self.state["posted_ids"] for i in role['ids'])):
            return True
        return any(same_role(k, posted)
                   for k in role['keys'] for posted in by_title.get(k.partition("|")[2], []))

    def forget_old(self, now_ts):
        cutoff = now_ts - REMEMBER_DAYS * 86400
        for table in ("posted_ids", "posted_keys", "posted_urls"):
            self.state[table] = {k: t for k, t in self.state[table].items() if t >= cutoff}

    # ---------- loop ----------

    @tasks.loop(minutes=CHECK_MINUTES)
    async def check_jobs(self):
        # A raised exception would stop tasks.loop for good, so catch everything
        async with self._lock:
            try:
                await self.run_check()
            except Exception:
                print("Job feed check failed:")
                traceback.print_exc()

    @check_jobs.before_loop
    async def before_check(self):
        await self.bot.wait_until_ready()

    async def load_listings(self, cutoff):
        """Open listings posted since 'cutoff' from every source. Curated lists come first, so their copy of a role wins."""
        boards_due = not self.boards_checked or time.monotonic() - self.boards_checked >= BOARD_CHECK_MINUTES * 60
        workday_due = not self.workday_checked or time.monotonic() - self.workday_checked >= WORKDAY_CHECK_MINUTES * 60
        request = {
            'cutoff': cutoff,
            'user_agent': USER_AGENT,
            # Boards and Workday list every role; only student ones are worth a closer look
            'student_filter': {'include': f"{INTERN}|{NEW_GRAD}", 'exclude': SENIOR},
            # Lets the worker drop off-topic listings before they ever reach this process
            'title_filter': {'include': list(TOPICS.values()), 'exclude': EXCLUDE},
            'lists': [
                {'source': source, 'track': track, 'url': url, 'etag': self.list_cache.get(url, (None, None))[0]}
                for source, track, url in GITHUB_LISTS
            ],
            'boards': [
                {'ats': ats, 'slug': slug, 'company': company, 'domain': domain}
                for ats, slug, company, domain in COMPANY_BOARDS
            ] if boards_due else [],
            'workday': [
                {'tenant': tenant, 'wd': wd, 'site': site, 'company': company, 'domain': domain}
                for tenant, wd, site, company, domain in WORKDAY_SITES
            ] if workday_due else [],
            'watch': self.watch_targets(include_workday=workday_due),
        }
        try:
            result = await run_worker(request)
        except Exception as e:
            print(f"Job worker failed, using cached listings: {e}")
        else:
            for entry in result['lists']:
                if entry['listings'] is not None:
                    self.list_cache[entry['url']] = (entry['etag'], entry['listings'])
            for entry in result['boards']:
                if entry['listings'] is not None:
                    self.board_cache[entry['slug']] = entry['listings']
            for entry in result.get('workday', []):
                if entry['listings'] is not None:
                    self.workday_cache[(entry['tenant'], entry['site'])] = entry['listings']
            self.status_cache.update(result.get('statuses', {}))
            if boards_due:
                self.boards_checked = time.monotonic()
            if workday_due:
                self.workday_checked = time.monotonic()

        listings = [job for _, jobs in self.list_cache.values() for job in jobs]
        listings += [job for jobs in self.board_cache.values() for job in jobs]
        return listings + [job for jobs in self.workday_cache.values() for job in jobs]

    async def run_check(self):
        channel = self.bot.get_channel(self.channel_id)
        if channel is None:
            try:
                channel = await self.bot.fetch_channel(self.channel_id)
            except discord.HTTPException as e:
                print(f"Error: Could not find jobs channel: {e}")
                return

        now_ts = int(discord.utils.utcnow().timestamp())
        cutoff = now_ts - MAX_AGE_DAYS * 86400
        if self.needs_history_sync:
            await self.sync_from_history(channel, cutoff)

        roles = select_jobs(await self.load_listings(cutoff), cutoff)
        if not self.state["posts_synced"]:
            await self.sync_posts(channel, roles, now_ts)

        by_title = self.posted_by_title()
        posts = self.post_index()
        new_roles = []
        for role in roles:
            if self.already_posted(role, by_title):
                # Still open: keep remembering it, along with any new ids or links it picked up
                self.mark_posted(role, now_ts)
                post = self.find_post(role, posts)
                if post:
                    merge_into_post(post, role)
            else:
                new_roles.append(role)
        new_roles.sort(key=lambda r: r['date_posted'])

        if new_roles:
            print(f"Job feed: posting {len(new_roles)} new listing(s) ({len(roles)} open from the last {MAX_AGE_DAYS} days).")
        for role in new_roles:
            logo = await self.get_logo(role['company_url']) or role['logo_fallback']
            try:
                message = await channel.send(embed=build_embed(role, logo))
            except discord.HTTPException as e:
                print(f"Failed to post job '{role['title']}' at {role['company']}: {e}")
                continue
            # Save right away so a crash can never cause a double post
            self.mark_posted(role, now_ts)
            self.track_post(role, message, now_ts)
            self.save_state()
            print(f"Posted job: {role['company']} - {role['title']}")
            await asyncio.sleep(POST_DELAY_SECONDS)

        await self.update_closed(now_ts)
        self.forget_old(now_ts)
        self.save_state()

    # ---------- closed roles ----------

    def watch_targets(self, include_workday):
        """Ids of every tracked post's role, links for posts that couldn't be matched to an id,
        and (when Workday is due) Workday links."""
        ids, links, workday = set(), set(), set()
        for post in self.state["posts"].values():
            ids.update(post["ids"])
            if not post["ids"]:
                links.update(post["raw_urls"])
            if include_workday:
                workday.update(u for u in post["raw_urls"] if is_workday_link(u))
        return {'ids': sorted(ids), 'urls': sorted(links), 'workday_urls': sorted(workday)}

    def post_index(self):
        index = {}
        for name, post in self.state["posts"].items():
            for value in post["ids"] + post["keys"] + [url_key(u) for u in post["raw_urls"]]:
                index[value] = name
        return index

    def find_post(self, role, index):
        for value in role['ids'] + role['keys'] + role['urls']:
            if value in index:
                return self.state["posts"].get(index[value])
        return None

    def track_post(self, role, message, now_ts):
        self.state["posts"][role['keys'][0]] = {
            "channel": message.channel.id, "message": message.id, "ids": list(role['ids']),
            "keys": list(role['keys']), "raw_urls": list(role['raw_urls']), "posted": now_ts, "closed": False,
        }

    async def sync_posts(self, channel, roles, now_ts):
        """Once: finds the posts already in the channel, so they're marked when their role closes."""
        roles_by = {}
        for role in roles:
            for value in role['urls'] + role['keys']:
                roles_by[value] = role
        after = datetime.datetime.fromtimestamp(now_ts - WATCH_DAYS * 86400, datetime.timezone.utc)
        found = 0
        try:
            async for message in channel.history(limit=None, after=after):
                if message.author.id != self.bot.user.id or not message.embeds:
                    continue
                embed = message.embeds[0]
                if not embed.url or not embed.title or "Job Feed" not in (embed.footer.text or ""):
                    continue
                title = embed.title.removeprefix(CLOSED_PREFIX)
                company = next((f.value for f in embed.fields if f.name == "Company"), "")
                key = dedupe_key(company, title)
                post = {
                    "channel": channel.id, "message": message.id, "ids": [], "keys": [key],
                    "raw_urls": [embed.url], "posted": int(message.created_at.timestamp()),
                    "closed": embed.title.startswith(CLOSED_PREFIX),
                }
                role = roles_by.get(url_key(embed.url)) or roles_by.get(key)
                if role:
                    merge_into_post(post, role)
                self.state["posts"][key] = post
                found += 1
        except discord.HTTPException as e:
            print(f"Could not read jobs channel history to track posts ({e}); will retry next check.")
            return
        self.state["posts_synced"] = True
        self.save_state()
        # Have the next check re-read every source in full, so these posts get statuses right away
        self.list_cache = {url: (None, jobs) for url, (_, jobs) in self.list_cache.items()}
        self.boards_checked = self.workday_checked = 0
        print(f"Job feed: tracking {found} existing post(s) for closing.")

    def post_status(self, post):
        """A role's company site outranks the aggregator lists when they disagree;
        within each, any source still showing the role open keeps it open."""
        direct = [self.status_cache.get(i) for i in post["ids"] if ":" in i]
        direct += [self.status_cache.get(u) for u in post["raw_urls"] if is_workday_link(u)]
        listed = [self.status_cache.get(i) for i in post["ids"] if ":" not in i]
        listed += [self.status_cache.get(u) for u in post["raw_urls"] if not is_workday_link(u)]
        for statuses in (direct, listed):
            if "open" in statuses:
                return "open"
            if "closed" in statuses:
                return "closed"
        return None

    async def update_closed(self, now_ts):
        for name, post in list(self.state["posts"].items()):
            status = self.post_status(post)
            if status is None or (status == "closed") == post["closed"]:
                continue
            if await self.set_closed(name, post, status == "closed"):
                self.save_state()
                await asyncio.sleep(POST_DELAY_SECONDS)
        cutoff = now_ts - WATCH_DAYS * 86400
        self.state["posts"] = {n: p for n, p in self.state["posts"].items() if p["posted"] >= cutoff}

    async def set_closed(self, name, post, closed):
        """Edits a post to show its role closed (or reopened). Returns whether it changed."""
        try:
            channel = self.bot.get_channel(post["channel"]) or await self.bot.fetch_channel(post["channel"])
            message = await channel.fetch_message(post["message"])
            embed = message.embeds[0]
            title = (embed.title or "").removeprefix(CLOSED_PREFIX)
            if closed:
                embed.title = (CLOSED_PREFIX + title)[:256]
                embed.description = CLOSED_TEXT
                embed.color = CLOSED_COLOR
            else:
                embed.title = title
                embed.description = f"[Apply here]({embed.url})"
                embed.color = IEEE_BLUE
            await message.edit(embed=embed)
        except discord.NotFound:
            # The post was deleted; nothing left to update
            self.state["posts"].pop(name, None)
            return False
        except (discord.HTTPException, IndexError) as e:
            print(f"Could not update the post for {name}: {e}")
            return False
        post["closed"] = closed
        print(f"Marked job {'closed' if closed else 'reopened'}: {title}")
        return True

    async def sync_from_history(self, channel, cutoff):
        """Marks the jobs this bot has already posted in the channel, so a lost or
        outdated state file can never cause a wave of reposts.

        Only messages since 'cutoff' can matter: anything still eligible opened after
        it, so its post (if any) was sent after it too.
        """
        now_ts = int(discord.utils.utcnow().timestamp())
        after = datetime.datetime.fromtimestamp(cutoff, datetime.timezone.utc)
        found = 0
        try:
            async for message in channel.history(limit=None, after=after):
                if message.author.id != self.bot.user.id:
                    continue
                for embed in message.embeds:
                    if not embed.url or not embed.title:
                        continue
                    company = next((f.value for f in embed.fields if f.name == "Company"), "")
                    self.state["posted_urls"][url_key(embed.url)] = now_ts
                    self.state["posted_keys"][dedupe_key(company, embed.title)] = now_ts
                    found += 1
        except discord.HTTPException as e:
            if "legacy" in self.state:
                # Can't tell what was posted, so fall back to the old state rather than repost
                legacy = self.state.pop("legacy")
                for table in ("posted_ids", "posted_keys", "posted_urls"):
                    self.state[table].update(legacy.get(table, {}))
            print(f"Could not read jobs channel history ({e}); relying on the state file.")
        else:
            self.state.pop("legacy", None)
            print(f"Job feed: found {found} job(s) already posted in the channel.")
        self.needs_history_sync = False
        self.save_state()

    async def get_logo(self, company_url):
        if not company_url:
            return None
        if company_url not in self.state["logos"]:
            self.state["logos"][company_url] = await fetch_company_logo(self.session, company_url)
        return self.state["logos"][company_url]


async def setup(bot):
    if not os.getenv('JOBS_CHANNEL_ID'):
        print("JOBS_CHANNEL_ID not set; job feed disabled.")
        return
    await bot.add_cog(JobFeed(bot))
