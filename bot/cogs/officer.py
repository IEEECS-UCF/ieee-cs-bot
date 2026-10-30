import os
import datetime

import aiohttp
import discord
from discord import app_commands
from discord.ext import commands, tasks

from bot.cogs import volunteer
from bot.utils.alerts import alert
from bot.officer import render, members, metrics
from bot.officer.data import (EventStore, migrate, load_json, save_json, NUDGES_FILE, DIGEST_STATE_FILE)
from bot.officer.sources import TZ, SnapshotCache, fetch_tabling, TABLING_SHEET_ID
from bot.officer.steps import event_steps, problems, owner_of
from bot.officer.views import (BoardView, CardView, ChapterView, OpenEventButton, open_card, open_buttons,
                               OFFICER_ROLE_IDS)

# The officer tools, built around one idea: every event has a status (its
# steps, in four phases), and every screen shows that same status.
#   /officer events   the board: every active event, one line each, and New event
#   /officer event    one event's card, with a menu that fixes each step
#   /officer chapter  deadlines, tabling, volunteer tasks, members, monthly report
# The digest (DIGEST_DAYS) and the daily nudges are drawn from the same steps.
# Last month's report posts on the 1st (METRICS_CHANNEL_ID).

WEEKDAYS = {"mon": 0, "tue": 1, "wed": 2, "thu": 3, "fri": 4, "sat": 5, "sun": 6}
DIGEST_DAYS = {WEEKDAYS[d.strip()[:3].lower()] for d in os.getenv('DIGEST_DAYS', 'sun,wed').split(",")
               if d.strip()[:3].lower() in WEEKDAYS}
DIGEST_HOUR = 10
NUDGE_HOUR = 11
NUDGE_REPEAT_DAYS = 7
REPORT_HOUR = 10
TABLING_TASK_DAYS = 14


def digest_due(now, last_day):
    """Post on a digest day, once, after DIGEST_HOUR."""
    return now.weekday() in DIGEST_DAYS and now.hour >= DIGEST_HOUR and last_day != now.date().isoformat()

REPORT_CATCH_UP_DAYS = 7  # if the bot was down on the 1st, post when it's back, within this many days

def report_due(now, posted):
    """Last month's key if its report should post now (from REPORT_HOUR on the 1st), else None."""
    key = metrics.month_key(*metrics.previous_month(now.year, now.month))
    started = now.day > 1 or now.hour >= REPORT_HOUR
    return key if started and now.day <= REPORT_CATCH_UP_DAYS and posted != key else None

def due_nudges(open_steps, sent, today):
    """{event id: [steps]} not mentioned in the last NUDGE_REPEAT_DAYS."""
    due = {}
    for gid, steps in open_steps.items():
        fresh = [s for s in steps if f"{gid}|{s.key}" not in sent
                 or (today - datetime.date.fromisoformat(sent[f"{gid}|{s.key}"])).days >= NUDGE_REPEAT_DAYS]
        if fresh:
            due[gid] = fresh
    return due


class Officer(commands.Cog):
    officer = app_commands.Group(
        name="officer", description="Officer tools (only you can see the replies).",
        default_permissions=discord.Permissions(manage_events=True),
    )

    def __init__(self, bot):
        self.bot = bot
        self.cache = SnapshotCache(bot)
        self.store = EventStore()
        bot.add_dynamic_items(OpenEventButton)
        self.digest_channel_id = int(os.getenv('OFFICER_DIGEST_CHANNEL_ID') or 0) or None
        self.nudge_channel_id = int(os.getenv('NUDGE_CHANNEL_ID') or 0) or None
        self.metrics_channel_id = int(os.getenv('METRICS_CHANNEL_ID') or 0) or None

    async def cog_load(self):
        sync = self.bot.get_cog("CalendarSync")
        moved = migrate({did: gid for gid, did in (sync.state["event_map"].items() if sync else [])})
        if moved:
            print(f"Officer tools: moved {moved} old data file(s) into data/events.json")
        self.warm.start()
        if self.digest_channel_id:
            self.officer_digest.start()
        else:
            print("Officer digest disabled: OFFICER_DIGEST_CHANNEL_ID not set.")
        if self.nudge_channel_id:
            self.daily_nudges.start()
        else:
            print("Event nudges disabled: NUDGE_CHANNEL_ID not set.")
        if self.metrics_channel_id:
            self.monthly_metrics.start()
        else:
            print("Monthly report disabled: METRICS_CHANNEL_ID not set.")

    def cog_unload(self):
        self.warm.cancel()
        self.officer_digest.cancel()
        self.daily_nudges.cancel()
        self.monthly_metrics.cancel()
        self.bot.remove_dynamic_items(OpenEventButton)

    async def interaction_check(self, interaction: discord.Interaction) -> bool:
        """Every /officer command: Officer or Exec role required."""
        roles = {role.id for role in getattr(interaction.user, 'roles', [])}
        if roles & OFFICER_ROLE_IDS:
            return True
        print(f"Refused /officer for {interaction.user} ({interaction.user.id}): no officer role")
        await interaction.response.send_message("These commands are for officers only.", ephemeral=True)
        return False

    # ---------- commands ----------

    @officer.command(name="events", description="Every active event and what's left to do.")
    async def events_cmd(self, interaction: discord.Interaction):
        await interaction.response.defer(ephemeral=True, thinking=True)
        snap = await self.cache.get()
        rows = render.board_rows(snap)
        await interaction.followup.send(embed=render.board_embed(snap, rows), view=BoardView(self, rows), ephemeral=True)

    async def event_autocomplete(self, interaction: discord.Interaction, current: str):
        snap = self.cache.snapshot
        if snap is None:
            return []
        return [app_commands.Choice(name=f"{render.fmt_day(e['start'])} · {e['name']}"[:100], value=e['id'])
                for e in sorted(snap.events, key=lambda e: e['start'])
                if current.casefold() in e['name'].casefold()][:25]

    @officer.command(name="event", description="One event's checklist, with a menu to fix anything on it.")
    @app_commands.describe(event="The event")
    @app_commands.autocomplete(event=event_autocomplete)
    async def event_cmd(self, interaction: discord.Interaction, event: str):
        await open_card(self, interaction, event)

    @officer.command(name="chapter", description="Deadlines, tabling, volunteer tasks, members and the monthly report.")
    async def chapter_cmd(self, interaction: discord.Interaction):
        await interaction.response.defer(ephemeral=True, thinking=True)
        view = ChapterView(self)
        view.message = await interaction.followup.send(embeds=await self.chapter_embeds(), view=view,
                                                       ephemeral=True, wait=True)

    # ---------- helpers the views use ----------

    async def chapter_embeds(self):
        rows = None
        if TABLING_SHEET_ID:
            try:
                async with aiohttp.ClientSession() as session:
                    rows = await fetch_tabling(session)
            except Exception as e:
                print(f"Tabling sheet unreadable: {e}")
        today = datetime.datetime.now(TZ).date()
        return render.chapter_embeds(rows, volunteer.load_tasks(), today, members.counts(today),
                                     os.getenv('GUILD_ID'))

    async def monthly_report(self, year, month):
        """(embeds, (csv bytes, filename)) for a month, comparing with the month before."""
        guild = self.bot.get_guild(int(os.getenv('GUILD_ID')))
        summary = metrics.summarize(await metrics.gather_month(year, month), year, month, guild)
        metrics.remember(summary)
        prev = metrics.previous_month(year, month)
        previous = metrics.remembered(*prev)
        if previous is None:
            previous = metrics.summarize(await metrics.gather_month(*prev), *prev, guild)
            previous.pop("discord_members", None)  # today's count, not that month's
            metrics.remember(previous)
        return metrics.report_embeds(summary, previous), metrics.csv_file(summary)

    def volunteer_channel(self):
        channel_id = os.getenv('VOLUNTEER_CHANNEL_ID')
        return self.bot.get_channel(int(channel_id)) if channel_id else None

    async def post_task(self, task):
        channel = self.volunteer_channel()
        if channel is None:
            return None
        message = await channel.send(embed=volunteer.task_embed(task), view=volunteer.task_view(task),
                                     allowed_mentions=discord.AllowedMentions.none())
        task["channel_id"], task["message_id"] = channel.id, message.id
        tasks_ = volunteer.load_tasks()
        tasks_[task["id"]] = task
        volunteer.save_tasks(tasks_)
        return message

    async def post_tabling_tasks(self, user_id):
        if self.volunteer_channel() is None or not TABLING_SHEET_ID:
            return "Needs VOLUNTEER_CHANNEL_ID and TABLING_SHEET_ID set."
        async with aiohttp.ClientSession() as session:
            rows = await fetch_tabling(session)
        existing = {t.get("key") for t in volunteer.load_tasks().values()}
        today = datetime.datetime.now(TZ).date()
        posted = []
        for r in rows:
            try:
                day = datetime.date.fromisoformat(r.get('Date', '').strip())
            except ValueError:
                continue
            filled = [r.get(k, '').strip() for k in ('Volunteer 1', 'Volunteer 2') if r.get(k, '').strip()]
            key = f"tabling|{day.isoformat()}|{r.get('Time', '').strip()}"
            if not 0 <= (day - today).days <= TABLING_TASK_DAYS or len(filled) >= 2 or key in existing:
                continue
            task = volunteer.new_task(
                f"Table: {r.get('Event Name', '').strip()} ({day:%a %b %-d})",
                f"{r.get('Time', '').strip()} · {r.get('Location', '').strip()}",
                (r.get('Notes', '').strip() + ("\nAlready signed up on the sheet: " + ", ".join(filled) if filled else "")).strip()
                + "\nAlso add your name to the Tabling Signup sheet.",
                2 - len(filled), user_id, key=key)
            message = await self.post_task(task)
            posted.append(f"{task['title']}: {message.jump_url}")
        return posted

    async def close_task(self, task_id):
        tasks_ = volunteer.load_tasks()
        task = tasks_.get(task_id)
        if task is None:
            return
        task["open"] = False
        volunteer.save_tasks(tasks_)
        channel = self.bot.get_channel(task["channel_id"]) if task["channel_id"] else None
        if channel:
            try:
                message = await channel.fetch_message(task["message_id"])
                await message.edit(embed=volunteer.task_embed(task), view=None)
            except discord.HTTPException as e:
                print(f"Could not update closed task message: {e}")

    # ---------- background ----------

    @tasks.loop(minutes=10)
    async def warm(self):
        """Keep the shared snapshot fresh so commands answer quickly."""
        try:
            await self.cache.get(fresh=True)
        except Exception as e:
            await alert(self.bot, "Officer data refresh", e)

    @warm.before_loop
    async def before_warm(self):
        await self.bot.wait_until_ready()

    @tasks.loop(minutes=30)
    async def officer_digest(self):
        try:
            await self.post_digest_if_due()
        except Exception as e:
            await alert(self.bot, "Officer digest", e)

    @officer_digest.before_loop
    async def before_digest(self):
        await self.bot.wait_until_ready()

    async def post_digest_if_due(self, force=False):
        now = datetime.datetime.now(TZ)
        state = load_json(DIGEST_STATE_FILE, {})
        if not force and not digest_due(now, state.get('day')):
            return
        channel = self.bot.get_channel(self.digest_channel_id)
        if channel is None:
            print("Error: Could not find officer digest channel.")
            return
        snap = await self.cache.get(fresh=True)
        rows = render.board_rows(snap, now)
        needs_work = [e for e, steps in rows if problems(steps)]
        embeds = [render.board_embed(snap, rows, problems_only=True)] + await self.chapter_embeds()
        role = os.getenv('OFFICER_DIGEST_ROLE_ID')
        batches = render.split_embeds(embeds)
        for i, batch in enumerate(batches):
            last = i == len(batches) - 1
            await channel.send(
                content=(f"<@&{role}> **Officer digest**" if role else "**Officer digest**") if i == 0 else None,
                embeds=batch, view=open_buttons(needs_work) if last and needs_work else None,
                allowed_mentions=discord.AllowedMentions(everyone=False, users=False,
                                                         roles=[discord.Object(int(role))] if role and i == 0 else False))
        save_json(DIGEST_STATE_FILE, {'day': now.date().isoformat()})

    @tasks.loop(minutes=30)
    async def monthly_metrics(self):
        try:
            await self.post_report_if_due()
        except Exception as e:
            await alert(self.bot, "Monthly report", e)

    @monthly_metrics.before_loop
    async def before_metrics(self):
        await self.bot.wait_until_ready()

    async def post_report_if_due(self, force_month=None):
        now = datetime.datetime.now(TZ)
        state = load_json(metrics.METRICS_FILE, {"months": {}})
        key = force_month or report_due(now, state.get("posted"))
        if key is None:
            return
        channel = self.bot.get_channel(self.metrics_channel_id)
        if channel is None:
            print("Error: Could not find the monthly report channel.")
            return
        embeds, file = await self.monthly_report(*map(int, key.split("-")))
        batches = render.split_embeds(embeds)
        for i, batch in enumerate(batches):
            last = i == len(batches) - 1
            await channel.send(content="**Monthly report**" if i == 0 else None, embeds=batch,
                               file=discord.File(*file) if last else discord.utils.MISSING,
                               allowed_mentions=discord.AllowedMentions.none())
        if force_month is None:
            state = load_json(metrics.METRICS_FILE, {"months": {}})
            state["posted"] = key
            save_json(metrics.METRICS_FILE, state)
        print(f"Posted the monthly report for {key}")

    @tasks.loop(minutes=30)
    async def daily_nudges(self):
        try:
            await self.send_nudges_if_due()
        except Exception as e:
            await alert(self.bot, "Event nudges", e)

    @daily_nudges.before_loop
    async def before_nudges(self):
        await self.bot.wait_until_ready()

    async def send_nudges_if_due(self, force=False):
        now = datetime.datetime.now(TZ)
        state = load_json(NUDGES_FILE, {"sent": {}, "threads": {}, "day": None})
        if not force and (now.hour < NUDGE_HOUR or state.get("day") == now.date().isoformat()):
            return
        channel = self.bot.get_channel(self.nudge_channel_id)
        if channel is None:
            print("Error: Could not find the nudge channel.")
            return
        snap = await self.cache.get(fresh=True)
        open_steps = {e['id']: problems(steps) for e, steps in render.board_rows(snap, now) if problems(steps)}
        due = due_nudges(open_steps, state["sent"], now.date())
        for gid, steps in due.items():
            event = snap.event(gid)
            owner, _ = owner_of(event, snap.store.get(gid, {}))
            thread = await self.event_thread(channel, event, state)
            view = discord.ui.View(timeout=None)
            view.add_item(OpenEventButton(gid))
            await thread.send(render.nudge_text(event, steps, owner), view=view,
                              allowed_mentions=discord.AllowedMentions(
                                  everyone=False, roles=False, users=[discord.Object(owner)] if owner else False))
            for step in steps:
                state["sent"][f"{gid}|{step.key}"] = now.date().isoformat()
            save_json(NUDGES_FILE, state)
            print(f"Nudged {event['name']}: {', '.join(s.key for s in steps)}")
        state["day"] = now.date().isoformat()
        save_json(NUDGES_FILE, state)

    async def event_thread(self, channel, event, state):
        """The event's own thread (forum post) in the nudge channel, created on first use."""
        thread_id = state["threads"].get(event['id'])
        if thread_id:
            thread = self.bot.get_channel(int(thread_id))
            if thread is None:
                try:
                    thread = await self.bot.fetch_channel(int(thread_id))
                except discord.HTTPException:
                    thread = None
            if thread is not None:
                if getattr(thread, 'archived', False):
                    await thread.edit(archived=False)
                return thread
        name = f"{event['name']} ({render.fmt_day(event['start'])})"[:100]
        if isinstance(channel, discord.ForumChannel):
            created = await channel.create_thread(name=name, content=f"Tracking **{event['name']}**: open items get posted here.")
            thread = created.thread
        else:
            starter = await channel.send(f"📌 **{event['name']}** ({render.fmt_day(event['start'])}): open items go in this thread.")
            thread = await starter.create_thread(name=name)
        state["threads"][event['id']] = thread.id
        save_json(NUDGES_FILE, state)
        return thread


async def setup(bot):
    await bot.add_cog(Officer(bot))
