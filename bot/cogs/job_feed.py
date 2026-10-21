import os
import re
import json
import asyncio
import datetime
import traceback

import discord
from discord.ext import commands, tasks
from bot.utils.job_listings import SOURCES, new_session, fetch_listings, fetch_company_logo
from bot.utils.formatting import create_base_embed, IEEE_BLUE

ORG_NAME = "IEEE CS"
STATE_FILE = "data/jobs_state.json"
CHECK_MINUTES = 30
MAX_AGE_DAYS = 3              # never announce anything older (e.g. after downtime)
MAX_POSTS_PER_RUN = 10        # leftovers go out on the next check
REMEMBER_DAYS = 60

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
EXCLUDE = r"\b(sales|marketing|recruit\w*|mba|accounting|accountant|finance|legal|business|designer|human resources|supply chain|procurement|technician|buyer)\b"

TOPIC_PATTERNS = {name: re.compile(rx, re.I) for name, rx in TOPICS.items()}
EXCLUDE_PATTERN = re.compile(EXCLUDE, re.I)

SEASONS = {"Summer": "☀️", "Fall": "🍂", "Winter": "❄️", "Spring": "🌱"}
SPONSORSHIP = {
    "Offers Sponsorship": "🛂 Offers visa sponsorship",
    "Does Not Offer Sponsorship": "🚫 No visa sponsorship",
    "U.S. Citizenship is Required": "🇺🇸 U.S. citizenship required",
}


def normalize(text):
    return " ".join((text or "").split())

def match_topics(title):
    if EXCLUDE_PATTERN.search(title):
        return []
    return [name for name, rx in TOPIC_PATTERNS.items() if rx.search(title)]

def dedupe_key(job):
    """Same company and title counts as one role, whatever the location."""
    return f"{normalize(job['company_name']).casefold()}|{normalize(job['title']).casefold()}"

def select_jobs(listings, track, now_ts):
    """Recent, open, on-topic listings, with per-location duplicates merged."""
    cutoff = now_ts - MAX_AGE_DAYS * 86400
    roles = {}
    for item in listings:
        if not item.get('active') or not item.get('is_visible', True):
            continue
        if item.get('date_posted', 0) < cutoff:
            continue
        topics = match_topics(item.get('title', ''))
        if not topics:
            continue

        key = dedupe_key(item)
        role = roles.get(key)
        if role is None:
            roles[key] = {
                'key': key,
                'ids': [item['id']],
                'track': track,
                'topics': topics,
                'title': normalize(item['title']),
                'company': normalize(item['company_name']),
                'company_url': item.get('company_url'),
                'url': item['url'],
                'locations': list(item.get('locations') or []),
                'terms': [t for t in item.get('terms', []) if t != 'N/A'],
                'sponsorship': item.get('sponsorship'),
                'degrees': item.get('degrees') or [],
                'date_posted': item['date_posted'],
            }
        else:
            role['ids'].append(item['id'])
            role['locations'] += [l for l in item.get('locations') or [] if l not in role['locations']]
            role['terms'] += [t for t in item.get('terms', []) if t != 'N/A' and t not in role['terms']]
            role['date_posted'] = min(role['date_posted'], item['date_posted'])
    return list(roles.values())

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
    if any(l.endswith(", FL") for l in role['locations']):
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
    # Simplify only records the posting date, so a timestamp would show a misleading time
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
    embed.set_footer(text=f"{ORG_NAME} Job Feed • via Simplify • Posted {posted.isoformat()}")
    return embed


class JobFeed(commands.Cog):
    """Posts new internship and new grad listings that match the club's fields of interest."""

    def __init__(self, bot):
        self.bot = bot
        self.channel_id = int(os.getenv('JOBS_CHANNEL_ID'))
        self.session = None
        self.cache = {}  # track -> (etag, listings)
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
        if os.path.exists(STATE_FILE):
            try:
                with open(STATE_FILE, "r") as f:
                    state = json.load(f)
            except (json.JSONDecodeError, OSError) as e:
                print(f"Job state file unreadable ({e}); moving it aside and starting fresh.")
                os.replace(STATE_FILE, STATE_FILE + ".corrupt")
                state = {}
        state.setdefault("initialized", False)
        state.setdefault("posted_ids", {})
        state.setdefault("posted_keys", {})
        state.setdefault("logos", {})
        return state

    def save_state(self):
        tmp = STATE_FILE + ".tmp"
        with open(tmp, "w") as f:
            json.dump(self.state, f, indent=2)
        os.replace(tmp, STATE_FILE)

    def mark_posted(self, role, now_ts):
        for job_id in role['ids']:
            self.state["posted_ids"][job_id] = now_ts
        self.state["posted_keys"][role['key']] = now_ts

    def already_posted(self, role):
        return role['key'] in self.state["posted_keys"] or any(
            job_id in self.state["posted_ids"] for job_id in role['ids']
        )

    def forget_old(self, now_ts):
        cutoff = now_ts - REMEMBER_DAYS * 86400
        for table in ("posted_ids", "posted_keys"):
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

    async def load_track(self, track, url):
        etag, listings = self.cache.get(track, (None, None))
        try:
            fresh, etag = await fetch_listings(self.session, url, etag)
        except Exception as e:
            print(f"Failed to fetch {track} listings: {e}")
            return listings or []
        if fresh is not None:
            # Only keep what could still be posted; the full files are ~13 MB each
            cutoff = discord.utils.utcnow().timestamp() - MAX_AGE_DAYS * 86400
            listings = [
                item for item in fresh
                if item.get('active') and item.get('date_posted', 0) >= cutoff
            ]
            self.cache[track] = (etag, listings)
        return listings or []

    async def run_check(self):
        channel = self.bot.get_channel(self.channel_id)
        if channel is None:
            try:
                channel = await self.bot.fetch_channel(self.channel_id)
            except discord.HTTPException as e:
                print(f"Error: Could not find jobs channel: {e}")
                return

        now_ts = int(discord.utils.utcnow().timestamp())
        roles = []
        for track, url in SOURCES.items():
            listings = await self.load_track(track, url)
            roles += select_jobs(listings, track, now_ts)

        new_roles = sorted(
            (r for r in roles if not self.already_posted(r)),
            key=lambda r: r['date_posted']
        )

        if not self.state["initialized"]:
            # First run: only announce the most recent few instead of the whole backlog
            backlog = new_roles[:-MAX_POSTS_PER_RUN] if len(new_roles) > MAX_POSTS_PER_RUN else []
            for role in backlog:
                self.mark_posted(role, now_ts)
            new_roles = new_roles[len(backlog):]
            self.state["initialized"] = True
            self.save_state()
            print(f"Job feed initialized; skipped {len(backlog)} older listings.")

        if not new_roles:
            self.forget_old(now_ts)
            self.save_state()
            return

        print(f"Job feed: {len(new_roles)} new listing(s), posting up to {MAX_POSTS_PER_RUN}.")
        for role in new_roles[:MAX_POSTS_PER_RUN]:
            logo = await self.get_logo(role['company_url'])
            try:
                await channel.send(embed=build_embed(role, logo))
            except discord.HTTPException as e:
                print(f"Failed to post job '{role['title']}' at {role['company']}: {e}")
                continue
            # Save right away so a crash can never cause a double post
            self.mark_posted(role, now_ts)
            self.save_state()
            print(f"Posted job: {role['company']} - {role['title']}")

        self.forget_old(now_ts)
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
