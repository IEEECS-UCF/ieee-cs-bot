import re
import sys
import html
import json
import asyncio
from urllib.parse import urlparse, parse_qs

import aiohttp

from bot.utils.job_worker import USER_AGENT

RAW = "https://raw.githubusercontent.com"

# Community-maintained lists in the SimplifyJobs listings.json format: (source, track, url)
GITHUB_LISTS = [
    ("Simplify", "Internship", f"{RAW}/SimplifyJobs/Summer2027-Internships/dev/.github/scripts/listings.json"),
    ("Simplify", "New Grad", f"{RAW}/SimplifyJobs/New-Grad-Positions/dev/.github/scripts/listings.json"),
    ("vanshb03", "Internship", f"{RAW}/vanshb03/Summer2027-Internships/dev/.github/scripts/listings.json"),
]

# Company career boards with public job APIs: (ats, board slug, company name, website domain)
COMPANY_BOARDS = [
    ("greenhouse", "spacex", "SpaceX", "spacex.com"),
    ("greenhouse", "andurilindustries", "Anduril", "anduril.com"),
    ("greenhouse", "tenstorrent", "Tenstorrent", "tenstorrent.com"),
    ("greenhouse", "lightmatter", "Lightmatter", "lightmatter.co"),
    ("greenhouse", "asteralabs", "Astera Labs", "asteralabs.com"),
    ("greenhouse", "graphcore", "Graphcore", "graphcore.ai"),
    ("greenhouse", "sambanovasystems", "SambaNova", "sambanova.ai"),
    ("greenhouse", "furiosaai", "FuriosaAI", "furiosa.ai"),
    ("greenhouse", "neuralink", "Neuralink", "neuralink.com"),
    ("greenhouse", "figureai", "Figure", "figure.ai"),
    ("greenhouse", "agilityrobotics", "Agility Robotics", "agilityrobotics.com"),
    ("greenhouse", "apptronik", "Apptronik", "apptronik.com"),
    ("greenhouse", "waymo", "Waymo", "waymo.com"),
    ("greenhouse", "rocketlab", "Rocket Lab", "rocketlabusa.com"),
    ("greenhouse", "relativity", "Relativity Space", "relativityspace.com"),
    ("greenhouse", "astranis", "Astranis", "astranis.com"),
    ("greenhouse", "epirus", "Epirus", "epirusinc.com"),
    ("greenhouse", "jumptrading", "Jump Trading", "jumptrading.com"),
    ("greenhouse", "towerresearchcapital", "Tower Research Capital", "tower-research.com"),
    ("greenhouse", "wehrtyou", "Hudson River Trading", "hudsonrivertrading.com"),
    ("greenhouse", "drweng", "DRW", "drw.com"),
    ("greenhouse", "optiverus", "Optiver", "optiver.com"),
    ("greenhouse", "imc", "IMC Trading", "imc.com"),
    ("greenhouse", "virtu", "Virtu Financial", "virtu.com"),
    ("greenhouse", "akunacapital", "Akuna Capital", "akunacapital.com"),
    ("ashby", "cerebras", "Cerebras", "cerebras.ai"),
    ("ashby", "etched", "Etched", "etched.com"),
    ("ashby", "d-matrix", "d-Matrix", "d-matrix.ai"),
    ("ashby", "matx", "MatX", "matx.com"),
    ("ashby", "skydio", "Skydio", "skydio.com"),
    ("ashby", "applied", "Applied Intuition", "appliedintuition.com"),
    ("lever", "zoox", "Zoox", "zoox.com"),
    ("lever", "hermeus", "Hermeus", "hermeus.com"),
]

WORKER_TIMEOUT = 300

_META_TAG = re.compile(r'<meta\s[^>]*>', re.I)
_ATTR = re.compile(r'(\w[\w:-]*)="([^"]*)"')


def new_session():
    return aiohttp.ClientSession(
        headers={"User-Agent": USER_AGENT},
        timeout=aiohttp.ClientTimeout(total=60),
    )

async def run_worker(request):
    """Fetches and slims down job sources in a child process; see bot.utils.job_worker."""
    proc = await asyncio.create_subprocess_exec(
        sys.executable, "-m", "bot.utils.job_worker",
        stdin=asyncio.subprocess.PIPE,
        stdout=asyncio.subprocess.PIPE,
        stderr=asyncio.subprocess.PIPE,
    )
    try:
        out, err = await asyncio.wait_for(proc.communicate(json.dumps(request).encode()), WORKER_TIMEOUT)
    except asyncio.TimeoutError:
        proc.kill()
        await proc.wait()
        raise RuntimeError(f"job worker took longer than {WORKER_TIMEOUT}s")
    if err:
        print(err.decode(errors="replace").strip())
    if proc.returncode != 0:
        raise RuntimeError(f"job worker exited with status {proc.returncode}")
    return json.loads(out)

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
