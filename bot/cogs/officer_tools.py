import os
import io
import csv
import json
import asyncio
import datetime
import difflib
import traceback
from zoneinfo import ZoneInfo

import aiohttp
import discord
from discord import app_commands
from discord.ext import commands, tasks
from dateutil import parser

from bot.utils.gcal_client import get_calendar_service
from bot.utils.formatting import create_base_embed, IEEE_BLUE, IEEE_GOLD
from bot.utils.vtools_client import fetch_branch_events
from bot.utils import checkin_client

TZ = ZoneInfo(os.getenv('REMINDER_TZ', 'America/New_York'))
BRANCH_SPOID = os.getenv('VTOOLS_SPOID', 'SBC13081')
TABLING_SHEET_ID = os.getenv('TABLING_SHEET_ID')
DEADLINES_FILE = "bot/deadlines.json"
DIGEST_STATE_FILE = "data/digest.json"
CHECKINS_FILE = "data/checkins.json"  # calendar event ID -> check-in form

VTOOLS_LOOKBACK_DAYS = 90
EVENTS_LOOKAHEAD_DAYS = 45
ROOM_WARNING_DAYS = 21     # flag events this close with no room
TABLING_LOOKAHEAD_DAYS = 60
DEADLINE_LOOKAHEAD_DAYS = 90
DIGEST_WEEKDAY = 0         # Monday
DIGEST_HOUR = 10           # local time, after the 9 AM weekly summary
CHECKIN_PICK_BACK_DAYS = 2    # events offered by /officer checkin create
CHECKIN_PICK_AHEAD_DAYS = 30

EMBED_LIMIT = 4000


# ---------- data sources ----------

def fetch_calendar(days_back, days_ahead):
    """Google Calendar events in a window, as (start, end, name, location, description)."""
    service = get_calendar_service()
    now = datetime.datetime.now(datetime.timezone.utc)
    items, token = [], None
    while True:
        result = service.events().list(
            calendarId=os.getenv('GOOGLE_CALENDAR_ID'),
            timeMin=(now - datetime.timedelta(days=days_back)).isoformat(),
            timeMax=(now + datetime.timedelta(days=days_ahead)).isoformat(),
            singleEvents=True, orderBy='startTime', maxResults=250, pageToken=token,
        ).execute(num_retries=3)
        items.extend(result.get('items', []))
        token = result.get('nextPageToken')
        if not token:
            break
    events = []
    for item in items:
        if item.get('status') == 'cancelled' or 'dateTime' not in item.get('start', {}):
            continue
        events.append({
            'id': item['id'],
            'start': parser.isoparse(item['start']['dateTime']).astimezone(TZ),
            'end': parser.isoparse(item['end']['dateTime']).astimezone(TZ),
            'name': " ".join((item.get('summary') or 'Untitled').split()),
            'location': " ".join((item.get('location') or '').split()),
            'description': (item.get('description') or '').strip(),
        })
    return events

async def fetch_tabling(session):
    """Rows of the Tabling Signup sheet (shared by link, read as CSV)."""
    url = f"https://docs.google.com/spreadsheets/d/{TABLING_SHEET_ID}/export?format=csv"
    async with session.get(url, timeout=aiohttp.ClientTimeout(total=60)) as resp:
        resp.raise_for_status()
        text = await resp.text()
    rows = list(csv.reader(io.StringIO(text)))
    header_at = next(i for i, r in enumerate(rows) if 'Event Name' in r)
    header = [h.strip() for h in rows[header_at]]
    return [dict(zip(header, r)) for r in rows[header_at + 1:] if any(c.strip() for c in r)]

def load_checkins():
    if not os.path.exists(CHECKINS_FILE):
        return {}
    with open(CHECKINS_FILE) as f:
        return json.load(f)

def save_checkins(checkins):
    tmp = CHECKINS_FILE + ".tmp"
    with open(tmp, "w") as f:
        json.dump(checkins, f, indent=2)
    os.replace(tmp, CHECKINS_FILE)

def load_deadlines():
    if not os.path.exists(DEADLINES_FILE):
        return []
    with open(DEADLINES_FILE) as f:
        return [(datetime.date.fromisoformat(d['date']), d['what']) for d in json.load(f)]


# ---------- checks (each returns lines of text) ----------

def fmt_day(dt):
    return dt.strftime('%a %b %-d')

def no_room(location):
    return location.casefold() in ('', 'tba', 'tbd')

def check_events(events):
    now = datetime.datetime.now(TZ)
    upcoming = [e for e in events if e['start'] > now]
    lines = []
    for e in upcoming:
        days = (e['start'].date() - now.date()).days
        issues = []
        if no_room(e['location']):
            issues.append("**no room**" if days <= ROOM_WARNING_DAYS else "no room yet")
        if not e['description']:
            issues.append("no description")
        for other in upcoming:
            if other is e or not (e['start'] < other['end'] and other['start'] < e['end']):
                continue
            same_room = e['location'] and e['location'].casefold() == other['location'].casefold()
            issues.append(f"**{'same room as' if same_room else 'overlaps'} {other['name']}**")
        due = []
        for what, lead in (("signage/IG", 14), ("announce", 7)):
            when = e['start'].date() - datetime.timedelta(days=lead)
            due.append(f"{what} {'was due' if when < now.date() else 'by'} {when:%b %-d}")
        status = "; ".join(issues) or "ok"
        lines.append(f"`{fmt_day(e['start'])}` **{e['name']}** ({e['location'] or 'TBA'}): {status}"
                     + (f"\n  ↳ {', '.join(due)}" if due else ""))
    return lines or ["No upcoming events in the calendar."]

async def checkin_note(event, checkins):
    """Check-in counts to copy into vTools, if the event had a check-in form."""
    form = checkins.get(event.get('id'))
    if not form or not checkin_client.configured():
        return ""
    try:
        r = await checkin_client.results(form['formId'])
    except Exception as e:
        return f" · check-in unavailable ({e})"
    return f" · check-in: **{r['ieee']} IEEE, {r['guests']} guests**"

async def check_vtools(events):
    now = datetime.datetime.now(TZ)
    checkins = load_checkins()
    # Tabling isn't an event we report
    past = [e for e in events if e['end'] < now and 'tabling' not in e['name'].casefold()]
    lines, cache = [], {}
    async with aiohttp.ClientSession() as session:
        for e in past:
            day = e['start'].date()
            if day not in cache:
                cache[day] = await fetch_branch_events(session, day, BRANCH_SPOID)
            match = best_match(e, cache[day])
            label = f"`{fmt_day(e['start'])}` {e['name']}"
            if match is None:
                lines.append(f"❌ {label}: **not in vTools**" + await checkin_note(e, checkins))
            elif match.get('ieee-attending') is None:
                lines.append(f"⚠️ {label}: in vTools, **no attendance filed** · <{match['link']}>"
                             + await checkin_note(e, checkins))
            else:
                ieee, guests = match['ieee-attending'], match.get('guests-attending') or 0
                lines.append(f"✅ {label}: {ieee + guests} ({ieee} IEEE, {guests} guests)")
    return lines or [f"No calendar events in the last {VTOOLS_LOOKBACK_DAYS} days."]

def best_match(event, candidates):
    """The vTools event closest in name, or in start time if names differ."""
    best, best_score = None, 0.0
    for c in candidates:
        start = parser.isoparse(c['start-time']).astimezone(TZ)
        if start.date() != event['start'].date():
            continue
        score = difflib.SequenceMatcher(None, event['name'].casefold(), c['title'].casefold()).ratio()
        if abs((start - event['start']).total_seconds()) <= 2 * 3600:
            score += 0.5
        if score > best_score:
            best, best_score = c, score
    return best if best_score >= 0.5 else None

async def check_tabling():
    if not TABLING_SHEET_ID:
        return ["Tabling sheet not configured (TABLING_SHEET_ID)."]
    async with aiohttp.ClientSession() as session:
        rows = await fetch_tabling(session)
    today = datetime.datetime.now(TZ).date()
    lines = []
    for r in rows:
        try:
            day = datetime.date.fromisoformat(r.get('Date', '').strip())
        except ValueError:
            continue
        if not (0 <= (day - today).days <= TABLING_LOOKAHEAD_DAYS):
            continue
        people = [r.get(k, '').strip() for k in ('Volunteer 1', 'Volunteer 2')]
        filled = [p for p in people if p]
        mark = "🔴" if not filled else ("🟡" if len(filled) < 2 else "🟢")
        who = ", ".join(filled) or "**nobody yet**"
        lines.append(f"{mark} `{day:%a %b %-d}` {r.get('Event Name', '').strip()} {r.get('Time', '').strip()}: {who}")
    return lines or ["No tabling sessions in the next two months."]

def check_deadlines():
    today = datetime.datetime.now(TZ).date()
    lines = []
    for day, what in sorted(load_deadlines()):
        left = (day - today).days
        if 0 <= left <= DEADLINE_LOOKAHEAD_DAYS:
            lines.append(f"`{day:%a %b %-d}` ({left}d) {what}")
    return lines or ["No deadlines in the next 90 days."]


def build_embed(title, lines, color=IEEE_BLUE, footer=None):
    text = ""
    for line in lines:
        if len(text) + len(line) + 1 > EMBED_LIMIT:
            text += "\n…"
            break
        text += line + "\n"
    embed = create_base_embed(title=title, description=text, color=color)
    if footer:
        embed.set_footer(text=footer)
    return embed


class OfficerTools(commands.Cog):
    """Officer checklists, shown privately to whoever runs the command.

    The Monday digest only posts if OFFICER_DIGEST_CHANNEL_ID is set.
    """

    officer = app_commands.Group(
        name="officer", description="Officer checklists (only you can see the replies).",
        default_permissions=discord.Permissions(manage_events=True),
    )

    checkin = app_commands.Group(
        name="checkin", description="Event check-in forms and attendance counts.", parent=officer,
    )

    def __init__(self, bot):
        self.bot = bot
        self._pick_cache = (None, [])  # (fetched at, events) for autocomplete
        channel = os.getenv('OFFICER_DIGEST_CHANNEL_ID')
        self.digest_channel_id = int(channel) if channel else None
        if self.digest_channel_id:
            self.weekly_digest.start()
        else:
            print("Officer digest disabled: OFFICER_DIGEST_CHANNEL_ID not set.")

    def cog_unload(self):
        self.weekly_digest.cancel()

    async def calendar(self, days_back, days_ahead):
        return await asyncio.to_thread(fetch_calendar, days_back, days_ahead)

    # ---------- commands ----------

    @officer.command(name="events", description="Upcoming events: missing rooms, clashes, promo due dates.")
    async def events_cmd(self, interaction: discord.Interaction):
        await interaction.response.defer(ephemeral=True, thinking=True)
        lines = check_events(await self.calendar(0, EVENTS_LOOKAHEAD_DAYS))
        await interaction.followup.send(embed=build_embed("📅 Upcoming events check", lines), ephemeral=True)

    @officer.command(name="vtools", description="Which past events are missing from vTools or have no attendance filed.")
    async def vtools_cmd(self, interaction: discord.Interaction):
        await interaction.response.defer(ephemeral=True, thinking=True)
        lines = await check_vtools(await self.calendar(VTOOLS_LOOKBACK_DAYS, 0))
        await interaction.followup.send(embed=build_embed(
            "🧾 vTools reporting", lines, IEEE_GOLD,
            footer="Checks Google Calendar events only; events made directly in Discord aren't covered."), ephemeral=True)

    @officer.command(name="tabling", description="Upcoming tabling sessions and who's signed up.")
    async def tabling_cmd(self, interaction: discord.Interaction):
        await interaction.response.defer(ephemeral=True, thinking=True)
        lines = await check_tabling()
        await interaction.followup.send(embed=build_embed("🪧 Tabling", lines), ephemeral=True)

    @officer.command(name="reminders", description="Preview the day-of event reminders for the next 7 days.")
    async def reminders_cmd(self, interaction: discord.Interaction):
        cog = self.bot.get_cog("EventReminders")
        lines = cog.preview_lines(interaction.guild) if cog else ["Event reminders aren't loaded."]
        await interaction.response.send_message(embed=build_embed("🔔 Event reminders", lines), ephemeral=True)

    @officer.command(name="deadlines", description="Chapter deadlines in the next 90 days.")
    async def deadlines_cmd(self, interaction: discord.Interaction):
        await interaction.response.send_message(embed=build_embed("⏰ Deadlines", check_deadlines(), IEEE_GOLD), ephemeral=True)

    # ---------- check-in ----------

    async def pickable_events(self):
        """Recent and upcoming calendar events, cached briefly for autocomplete."""
        fetched, events = self._pick_cache
        now = datetime.datetime.now(TZ)
        if fetched is None or (now - fetched).total_seconds() > 300:
            events = await self.calendar(CHECKIN_PICK_BACK_DAYS, CHECKIN_PICK_AHEAD_DAYS)
            self._pick_cache = (now, events)
        return events

    async def event_autocomplete(self, interaction: discord.Interaction, current: str):
        events = await self.pickable_events()
        return [app_commands.Choice(name=f"{fmt_day(e['start'])} · {e['name']}"[:100], value=e['id'])
                for e in events if current.casefold() in e['name'].casefold()][:25]

    async def form_autocomplete(self, interaction: discord.Interaction, current: str):
        forms = sorted(load_checkins().items(), key=lambda kv: kv[1]['start'], reverse=True)
        return [app_commands.Choice(name=f"{form['day']} · {form['name']}"[:100], value=key)
                for key, form in forms if current.casefold() in form['name'].casefold()][:25]

    @checkin.command(name="create", description="Make (or fetch) the check-in form and QR code for an event.")
    @app_commands.describe(event="The calendar event")
    @app_commands.autocomplete(event=event_autocomplete)
    async def checkin_create(self, interaction: discord.Interaction, event: str):
        await interaction.response.defer(ephemeral=True, thinking=True)
        if not checkin_client.configured():
            await interaction.followup.send("Check-in isn't configured (CHECKIN_URL / CHECKIN_SECRET).", ephemeral=True)
            return
        match = next((e for e in await self.pickable_events() if e['id'] == event), None)
        if match is None:
            await interaction.followup.send("Pick an event from the list.", ephemeral=True)
            return
        day = fmt_day(match['start'])
        try:
            form = await checkin_client.create_form(match['name'], day, match['id'])
        except checkin_client.CheckinError as e:
            await interaction.followup.send(f"The check-in script returned an error: {e}", ephemeral=True)
            return
        checkins = load_checkins()
        checkins[match['id']] = {'formId': form['formId'], 'url': form['url'], 'shortUrl': form['shortUrl'],
                                 'name': match['name'], 'day': day, 'start': match['start'].isoformat()}
        save_checkins(checkins)

        link = form['shortUrl'] or form['url']
        qr = discord.File(io.BytesIO(checkin_client.qr_png(link)), filename="checkin-qr.png")
        embed = create_base_embed(
            title=f"✅ Check-in: {match['name']}",
            description=(f"**Form:** {link}\n\nPut the QR on a slide or print it for the door. "
                         f"Afterwards, run `/officer checkin results` for the vTools numbers."
                         + ("\n\n*This form already existed; same link as before.*" if form.get('existed') else "")),
            color=IEEE_BLUE)
        embed.set_image(url="attachment://checkin-qr.png")
        await interaction.followup.send(embed=embed, file=qr, ephemeral=True)

    @checkin.command(name="results", description="Attendance from an event's check-in form (IEEE vs. guests for vTools).")
    @app_commands.describe(event="An event with a check-in form")
    @app_commands.autocomplete(event=form_autocomplete)
    async def checkin_results(self, interaction: discord.Interaction, event: str):
        await interaction.response.defer(ephemeral=True, thinking=True)
        form = load_checkins().get(event)
        if form is None:
            await interaction.followup.send("No check-in form for that event. Pick one from the list.", ephemeral=True)
            return
        try:
            r = await checkin_client.results(form['formId'])
        except checkin_client.CheckinError as e:
            await interaction.followup.send(f"The check-in script returned an error: {e}", ephemeral=True)
            return
        lines = [
            f"**Checked in:** {r['total']}",
            f"**For vTools:** IEEE attending **{r['ieee']}**, guests **{r['guests']}**",
        ]
        if r['notSure']:
            lines.append(f"({r['notSure']} answered \"Not sure\" about IEEE membership and are counted as guests.)")
        heard = sorted(r['heard'].items(), key=lambda kv: -kv[1])
        if heard:
            lines.append("\n**How they heard about it:**")
            lines += [f"{source}: {count}" for source, count in heard]
        lines.append(f"\nForm: {form['shortUrl'] or form['url']}")
        await interaction.followup.send(embed=build_embed(f"🧾 Check-in results: {form['name']} ({form['day']})", lines, IEEE_GOLD),
                                        ephemeral=True)

    # ---------- Monday digest (off unless configured) ----------

    @tasks.loop(minutes=30)
    async def weekly_digest(self):
        # A raised exception would stop tasks.loop for good, so catch everything
        try:
            await self.post_digest_if_due()
        except Exception:
            print("Officer digest failed:")
            traceback.print_exc()

    @weekly_digest.before_loop
    async def before_digest(self):
        await self.bot.wait_until_ready()

    async def post_digest_if_due(self):
        now = datetime.datetime.now(TZ)
        week = now.strftime('%G-W%V')
        state = {}
        if os.path.exists(DIGEST_STATE_FILE):
            with open(DIGEST_STATE_FILE) as f:
                state = json.load(f)
        if now.weekday() != DIGEST_WEEKDAY or now.hour < DIGEST_HOUR or state.get('week') == week:
            return
        channel = self.bot.get_channel(self.digest_channel_id)
        if channel is None:
            print("Error: Could not find officer digest channel.")
            return
        events = await self.calendar(VTOOLS_LOOKBACK_DAYS, EVENTS_LOOKAHEAD_DAYS)
        embeds = [
            build_embed("📅 Upcoming events check", check_events(events)),
            build_embed("🧾 vTools reporting", [l for l in await check_vtools(events) if not l.startswith("✅")] or ["All caught up."], IEEE_GOLD),
            build_embed("🪧 Tabling", await check_tabling()),
            build_embed("⏰ Deadlines", check_deadlines(), IEEE_GOLD),
        ]
        role = os.getenv('OFFICER_DIGEST_ROLE_ID')
        content = f"<@&{role}> **Weekly officer digest**" if role else "**Weekly officer digest**"
        await channel.send(content=content, embeds=embeds, allowed_mentions=discord.AllowedMentions(
            everyone=False, users=False, roles=[discord.Object(int(role))] if role else False))
        tmp = DIGEST_STATE_FILE + ".tmp"
        with open(tmp, "w") as f:
            json.dump({'week': week}, f)
        os.replace(tmp, DIGEST_STATE_FILE)


async def setup(bot):
    await bot.add_cog(OfficerTools(bot))
