import io
import os
import re
import asyncio
import datetime

import aiohttp
import discord

from bot.utils import event_text, checkin_client
from bot.utils import knightconnect_client as kc
from bot.utils.vtools_client import fetch_event as fetch_vtools_event, parse_event_id
from bot.utils.gcal_client import patch_event, insert_event, delete_event as delete_calendar_event
from bot.utils.event_text import same_place
from bot.utils.formatting import create_base_embed, IEEE_BLUE, IEEE_GOLD
from bot.cogs import volunteer, event_reminders
from bot.officer.data import EventStore, SKIPPED, add_deadline, remove_deadline, load_deadlines, deadline_key
from bot.officer.sources import TZ, fetch_repos, fetch_tabling, to_event, TABLING_SHEET_ID, WINDOW_AHEAD_DAYS
from bot.officer.steps import event_steps, owner_of, booked_room, clashes, PROBLEMS
from bot.officer import render, new_event

# The interactive parts of the officer tools. Every view here is ephemeral
# (only the officer who opened it sees it) except OpenEventButton, which sits
# on nudges and the digest and checks the officer role itself.

OFFICER_ROLE_IDS = {int(r) for r in os.getenv('OFFICER_ROLE_IDS', '1449921163248533525,1424610929424994477').split(',') if r.strip()}
VIEW_TIMEOUT = 840  # Discord lets the bot edit an ephemeral message for 15 minutes
store = EventStore()

ACTIONS = [
    # (action key, menu label, description)
    ("edit", "Edit details", "Title, date, time, description"),
    ("owner", "Set owner", "Who gets this event's nudges"),
    ("room", "Room request", "Requested, confirmed or denied"),
    ("copy:knightconnect", "KnightConnect: copy fields", "Laid out for KnightConnect's form"),
    ("link:knightconnect", "KnightConnect: link listing", "Fix the match, or mark not needed"),
    ("copy:vtools", "vTools: copy fields", "Laid out for vTools' create form"),
    ("link:vtools", "vTools: link listing", "Fix the match, or mark not needed"),
    ("draft", "Draft announcement", "#announcements post + Instagram caption"),
    ("reminder", "Reminder settings", "Send or skip, and who it pings"),
    ("checkin", "Check-in form & QR", "Create it, or see results"),
    ("attendance", "Attendance for vTools", "Counts to type into the vTools report"),
    ("slides", "Link slides", "A repo or any link"),
    ("mark", "Mark a step", "Done, not needed, or undo"),
    ("delete", "Delete event", "From Google Calendar and Discord"),
]
MARKABLE = [("description", "Description"), ("flyer", "Flyer"), ("announcement", "Announcement"),
            ("checkin", "Check-in"), ("slides", "Slides"), ("recap", "Recap post")]


def is_officer(user):
    return bool({role.id for role in getattr(user, 'roles', [])} & OFFICER_ROLE_IDS)

def mentions_in(text):
    """Allow exactly the mentions written into an approved post, nothing else."""
    roles = [discord.Object(int(r)) for r in re.findall(r'<@&(\d+)>', text)]
    return discord.AllowedMentions(everyone='@everyone' in text, users=False, roles=roles or False)


# ---------- opening a card ----------

async def open_card(cog, interaction, gid):
    """Send the event card for gid as a new ephemeral message."""
    if not interaction.response.is_done():
        await interaction.response.defer(ephemeral=True, thinking=True)
    snap = await cog.cache.get()
    event = snap.event(gid)
    if event is None:
        await interaction.followup.send("That event isn't in the calendar window anymore.", ephemeral=True)
        return
    view = CardView(cog, gid)
    view.message = await interaction.followup.send(
        embed=render.card_embed(event, event_steps(event, snap), snap), view=view, ephemeral=True, wait=True)


class CardView(discord.ui.View):
    def __init__(self, cog, gid):
        super().__init__(timeout=VIEW_TIMEOUT)
        self.cog, self.gid, self.message = cog, gid, None
        menu = discord.ui.Select(placeholder="Do something…", options=[
            discord.SelectOption(label=label, value=key, description=desc) for key, label, desc in ACTIONS])
        menu.callback = self.on_action
        self.add_item(menu)

    async def redraw(self, fresh=False):
        snap = await self.cog.cache.get(fresh=True) if fresh else self.cog.cache.with_store()
        event = snap.event(self.gid)
        if event and self.message:
            try:
                await self.message.edit(embed=render.card_embed(event, event_steps(event, snap), snap), view=self)
            except discord.HTTPException as e:
                print(f"Could not redraw event card: {e}")

    @discord.ui.button(label="↻ Refresh", style=discord.ButtonStyle.secondary, row=1)
    async def refresh(self, interaction, button):
        await interaction.response.defer()
        await self.redraw(fresh=True)

    async def on_action(self, interaction):
        action = interaction.data["values"][0]
        event = self.cog.cache.with_store().event(self.gid)
        handler = {
            "owner": self.do_owner, "room": self.do_room, "draft": self.do_draft, "reminder": self.do_reminder,
            "checkin": self.do_checkin, "attendance": self.do_attendance, "slides": self.do_slides, "mark": self.do_mark,
            "edit": self.do_edit, "delete": self.do_delete,
        }.get(action)
        if event is None and action in ("edit", "delete"):
            await interaction.response.send_message("That event isn't in the calendar anymore.", ephemeral=True)
        elif action.startswith("copy:"):
            await self.do_copy(interaction, event, action.split(":")[1])
        elif action.startswith("link:"):
            await interaction.response.send_modal(LinkModal(self, action.split(":")[1]))
        elif handler:
            await handler(interaction, event)

    # each action ----------

    async def do_owner(self, interaction, event):
        view = OwnerPicker(self)
        await interaction.response.send_message("Pick the owner, or reset to the default:", view=view, ephemeral=True)

    async def do_room(self, interaction, event):
        await interaction.response.send_message("Where does the room request stand?", view=RoomPicker(self, event), ephemeral=True)

    async def do_copy(self, interaction, event, platform):
        await interaction.response.defer(ephemeral=True, thinking=True)
        name = "KnightConnect" if platform == "knightconnect" else "vTools"
        embed = create_base_embed(title=f"📋 {name}: {event['name']}",
                                  description=event_text.copy_block(event, platform), color=IEEE_BLUE)
        files = []
        sync = self.cog.bot.get_cog("CalendarSync")
        if platform == "knightconnect" and event.get('flyer') and sync:
            image = await sync.download_flyer(event['flyer'])
            if image:
                files.append(discord.File(io.BytesIO(image), filename="flyer.png"))
                embed.set_image(url="attachment://flyer.png")
        await interaction.followup.send(embed=embed, files=files, ephemeral=True)

    async def do_draft(self, interaction, event):
        role_key = event_text.kind(event)
        role_id = os.getenv(event_reminders.ROLE_ENV.get(role_key, ''), '')
        did = self.cog.cache.with_store().discord_ids.get(self.gid)
        link = f"https://discord.com/events/{os.getenv('GUILD_ID')}/{did}" if did else ""
        draft = event_text.announcement_draft(event, link, f"<@&{role_id}>" if role_id else None)
        caption = event_text.instagram_caption(event)
        text = (f"**#announcements draft** (mentions only ping when posted)\n```md\n{draft}\n```"
                f"\n**Instagram caption**\n```\n{caption}\n```")
        channel_id = os.getenv('ANNOUNCE_CHANNEL_ID') or os.getenv('REMINDER_CHANNEL_ID')
        view = DraftView(self, draft, int(channel_id)) if channel_id else None
        await interaction.response.send_message(text[:2000], view=view, ephemeral=True)

    async def do_reminder(self, interaction, event):
        await interaction.response.send_message("Day-of reminder for this event:", view=ReminderPicker(self), ephemeral=True)

    async def do_checkin(self, interaction, event):
        await interaction.response.defer(ephemeral=True, thinking=True)
        if not checkin_client.configured():
            await interaction.followup.send("Check-in isn't configured (CHECKIN_URL / CHECKIN_SECRET).", ephemeral=True)
            return
        form = store.get(self.gid).get('checkin')
        try:
            if form is None:
                created = await checkin_client.create_form(event['name'], render.fmt_day(event['start']), self.gid)
                form = {k: created[k] for k in ("formId", "url", "shortUrl")} | {"linked": False}
                store.set_checkin(self.gid, form)
            results = await checkin_client.results(form['formId'])
        except checkin_client.CheckinError as e:
            await interaction.followup.send(f"The check-in script returned an error: {e}", ephemeral=True)
            return
        snap = self.cog.cache.with_store()
        snap.checkins[self.gid] = results
        link = form.get('shortUrl') or form['url']
        lines = [f"**Form:** {link}", f"**Checked in:** {results['total']} ({results['ieee']} IEEE, {results['guests']} guests)"]
        heard = sorted(results['heard'].items(), key=lambda kv: -kv[1])
        if heard:
            lines.append("**How they heard:** " + ", ".join(f"{s} {n}" for s, n in heard))
        embed = create_base_embed(title=f"✅ Check-in: {event['name']}", description="\n".join(lines), color=IEEE_BLUE)
        embed.set_image(url="attachment://checkin-qr.png")
        qr = discord.File(io.BytesIO(checkin_client.qr_png(link)), filename="checkin-qr.png")
        await interaction.followup.send(embed=embed, file=qr, ephemeral=True)
        await self.redraw()

    async def do_attendance(self, interaction, event):
        snap = self.cog.cache.with_store()
        counts, record = snap.checkins.get(self.gid, {}), snap.vtools.get(self.gid)
        lines = []
        if 'total' in counts:
            lines.append(f"From check-in: **IEEE attending {counts['ieee']}**, **guests {counts['guests']}**"
                         + (f" ({counts['notSure']} unsure counted as guests)" if counts.get('notSure') else ""))
        else:
            lines.append("No check-in form for this event, so count by hand.")
        if isinstance(record, dict):
            lines.append(f"File it on the vTools event: https://events.vtools.ieee.org/tego_/event/manage/{record['id']} "
                         "(Report Info → IEEE Attended / Guests Attended)")
        else:
            lines.append("There's no vTools event yet: create one (menu → vTools: copy fields), then file the report.")
        await interaction.response.send_message("\n".join(lines), ephemeral=True)

    async def do_slides(self, interaction, event):
        await interaction.response.send_modal(SlidesModal(self))

    async def do_mark(self, interaction, event):
        await interaction.response.send_message("Which step?", view=MarkPicker(self), ephemeral=True)

    async def do_edit(self, interaction, event):
        await interaction.response.send_modal(EditModal(self, event))

    async def do_delete(self, interaction, event):
        past = event['start'] < datetime.datetime.now(TZ)
        text = (f"Delete **{event['name']}** ({render.fmt_day(event['start'])})? It comes off Google Calendar now "
                "(kept in the calendar's trash for 30 days) and off Discord within a few minutes."
                + ("\n⚠️ It already happened, so it would also drop out of the monthly report." if past else ""))
        await interaction.response.send_message(text, view=DeleteConfirm(self, event), ephemeral=True)


# ---------- action pickers and forms ----------

class EditModal(discord.ui.Modal, title="Edit event"):
    """Changes the calendar event; calendar sync carries it to Discord."""
    def __init__(self, card, event):
        super().__init__(timeout=VIEW_TIMEOUT)
        self.card, self.event = card, event
        start, end = event['start'], event['end']
        self.name = discord.ui.TextInput(label="Title", max_length=100, default=event['name'])
        self.date = discord.ui.TextInput(label="Date", max_length=30, default=f"{start:%m/%d/%Y}")
        self.time = discord.ui.TextInput(label="Time", max_length=40, default=f"{start:%-I:%M %p}-{end:%-I:%M %p}")
        for item in (self.name, self.date, self.time):
            self.add_item(item)
        # Discord caps a text box at 4000 characters; longer descriptions are edited in the calendar
        self.description = None
        if len(event['description']) <= 4000:
            self.description = discord.ui.TextInput(label="Description", style=discord.TextStyle.paragraph, required=False,
                                                    max_length=4000, default=event['description'] or None)
            self.add_item(self.description)

    async def on_submit(self, interaction):
        e = self.event
        try:
            start, end = new_event.parse_form(self.date.value, self.time.value, datetime.datetime.now(TZ))
        except ValueError as err:
            await interaction.response.send_message(f"{err} Nothing was changed.", ephemeral=True)
            return
        body, changes = {}, []
        name = " ".join(self.name.value.split())
        if name != e['name']:
            body['summary'] = name
            changes.append(f"title → **{name}**")
        if start != e['start'] or end != e['end']:
            body['start'] = {"dateTime": start.isoformat(), "timeZone": str(TZ)}
            body['end'] = {"dateTime": end.isoformat(), "timeZone": str(TZ)}
            changes.append(f"time → **{start:%a %b %-d, %-I:%M %p} – {end:%-I:%M %p}**")
        if self.description is not None and self.description.value.strip() != e['description']:
            body['description'] = self.description.value.strip()
            changes.append("description")
        if not body:
            await interaction.response.send_message("Nothing changed.", ephemeral=True)
            return
        await interaction.response.defer(ephemeral=True, thinking=True)
        try:
            await asyncio.to_thread(patch_event, os.getenv('GOOGLE_CALENDAR_ID'), self.card.gid, body)
        except Exception as err:
            await interaction.followup.send(f"Couldn't update Google Calendar ({err}). Nothing was changed.", ephemeral=True)
            return
        sync = interaction.client.get_cog("CalendarSync")
        if sync:
            sync.sync_soon(5)
        print(f"Edited event {e['name']} ({self.card.gid}) by {interaction.user} ({interaction.user.id}): {list(body)}")
        lines = ["Updated " + ", ".join(changes) + ". Discord follows within a few minutes."]
        snap = await self.card.cog.cache.get(fresh=True)
        if 'start' in body:
            room = booked_room(e, snap.store.get(self.card.gid, {}))
            lines += render.clash_lines(clashes(snap, start, end, room, exclude=self.card.gid))
        await interaction.followup.send("\n".join(lines), ephemeral=True)
        await self.card.redraw()


class DeleteConfirm(discord.ui.View):
    def __init__(self, card, event):
        super().__init__(timeout=VIEW_TIMEOUT)
        self.card, self.event = card, event

    @discord.ui.button(label="Delete", style=discord.ButtonStyle.danger)
    async def delete(self, interaction, button):
        if self.is_finished():
            await interaction.response.send_message("Already handled.", ephemeral=True)
            return
        self.stop()
        e = self.event
        try:
            await asyncio.to_thread(delete_calendar_event, os.getenv('GOOGLE_CALENDAR_ID'), self.card.gid)
        except Exception as err:
            await interaction.response.edit_message(content=f"Couldn't delete it from Google Calendar ({err}). Nothing was deleted.",
                                                    view=None)
            return
        sync = interaction.client.get_cog("CalendarSync")
        if sync:
            sync.sync_soon(5)
        snap = self.card.cog.cache.with_store()
        if snap is not None:
            snap.events = [x for x in snap.events if x['id'] != self.card.gid]
        print(f"Deleted event {e['name']} ({self.card.gid}) by {interaction.user} ({interaction.user.id})")
        await interaction.response.edit_message(
            content=f"Deleted **{e['name']}** from Google Calendar; the Discord event goes within a few minutes. "
                    "To undo, restore it from the calendar's trash.", view=None)
        if self.card.message:
            try:
                await self.card.message.edit(content=f"~~{e['name']}~~ was deleted.", embed=None, view=None)
            except discord.HTTPException:
                pass

    @discord.ui.button(label="Keep it", style=discord.ButtonStyle.secondary)
    async def keep(self, interaction, button):
        self.stop()
        await interaction.response.edit_message(content="Kept; nothing was deleted.", view=None)

class OwnerPicker(discord.ui.View):
    def __init__(self, card):
        super().__init__(timeout=VIEW_TIMEOUT)
        self.card = card

    @discord.ui.select(cls=discord.ui.UserSelect, placeholder="Choose an officer")
    async def pick(self, interaction, select):
        store.set_owner(self.card.gid, select.values[0].id)
        await interaction.response.edit_message(content=f"Owner set to {select.values[0].mention}.", view=None)
        await self.card.redraw()

    @discord.ui.button(label="Reset to default", style=discord.ButtonStyle.secondary)
    async def reset(self, interaction, button):
        store.set_owner(self.card.gid, None)
        await interaction.response.edit_message(content="Owner reset to the default.", view=None)
        await self.card.redraw()


class RoomModal(discord.ui.Modal):
    def __init__(self, card, status, current):
        super().__init__(title="Room request" if status == "requested" else "Confirmed room", timeout=VIEW_TIMEOUT)
        self.card, self.status = card, status
        self.room = discord.ui.TextInput(label="Room", placeholder="e.g. ENG2 205", default=current or None, max_length=100)
        self.note = discord.ui.TextInput(label="Note (optional)", required=False, max_length=200,
                                         placeholder="Who you asked, request number…")
        self.add_item(self.room)
        self.add_item(self.note)

    async def on_submit(self, interaction):
        room = " ".join(self.room.value.split())
        store.set_room(self.card.gid, self.status, room, interaction.user.id, self.note.value)
        lines = [f"Room {self.status}: **{room}**."]
        event = self.card.cog.cache.with_store().event(self.card.gid)
        if self.status == "confirmed" and event and not same_place(room, event['location'] or ''):
            try:
                await asyncio.to_thread(patch_event, os.getenv('GOOGLE_CALENDAR_ID'), self.card.gid, {'location': room})
                event['location'] = room
                lines.append("Updated the calendar's location; Discord follows within a few minutes.")
            except Exception as e:
                lines.append(f"Couldn't update the calendar's location ({e}); change it there by hand.")
        snap = self.card.cog.cache.with_store()
        if event:
            mine = booked_room(event, snap.store.get(self.card.gid, {}))
            lines += render.clash_lines(clashes(snap, event['start'], event['end'], mine, exclude=self.card.gid),
                                        same_room_only=True)
        await interaction.response.send_message("\n".join(lines), ephemeral=True)
        await self.card.redraw()


class RoomPicker(discord.ui.View):
    def __init__(self, card, event):
        super().__init__(timeout=VIEW_TIMEOUT)
        self.card, self.event = card, event

    def current(self):
        room = store.get(self.card.gid).get('room', {}).get('room')
        return room or (self.event['location'] if self.event and self.event['location'] not in ('', 'TBA') else None)

    @discord.ui.button(label="Requested…", style=discord.ButtonStyle.primary)
    async def requested(self, interaction, button):
        await interaction.response.send_modal(RoomModal(self.card, "requested", self.current()))

    @discord.ui.button(label="Confirmed…", style=discord.ButtonStyle.success)
    async def confirmed(self, interaction, button):
        await interaction.response.send_modal(RoomModal(self.card, "confirmed", self.current()))

    @discord.ui.button(label="Denied", style=discord.ButtonStyle.danger)
    async def denied(self, interaction, button):
        store.set_room(self.card.gid, "denied", self.current() or "the requested room", interaction.user.id)
        await interaction.response.edit_message(content="Marked the room request as denied.", view=None)
        await self.card.redraw()

    @discord.ui.button(label="Clear", style=discord.ButtonStyle.secondary)
    async def clear(self, interaction, button):
        store.set_room(self.card.gid, "clear", None, interaction.user.id)
        await interaction.response.edit_message(content="Room request cleared.", view=None)
        await self.card.redraw()


class LinkModal(discord.ui.Modal):
    def __init__(self, card, platform):
        self.platform = platform
        self.name = "KnightConnect" if platform == "knightconnect" else "vTools"
        super().__init__(title=f"{self.name} listing", timeout=VIEW_TIMEOUT)
        self.card = card
        self.value = discord.ui.TextInput(label="Listing link or ID", max_length=300,
                                          placeholder="link or ID · auto (match again) · not needed")
        self.add_item(self.value)

    async def on_submit(self, interaction):
        await interaction.response.defer(ephemeral=True, thinking=True)
        text = self.value.value.strip()
        snap = self.card.cog.cache.with_store()
        matches = snap.knightconnect if self.platform == "knightconnect" else snap.vtools
        if text.casefold() == "auto":
            store.set_link(self.platform, self.card.gid, None)
            note = "Cleared; it will be matched automatically on the next refresh."
            await self.card.cog.cache.get(fresh=True)
        elif text.casefold() in ("not needed", "none", "skip"):
            store.set_link(self.platform, self.card.gid, SKIPPED)
            matches[self.card.gid] = SKIPPED
            note = f"Marked as not needing a {self.name} listing."
        else:
            listing_id = parse_event_id(text)
            async with aiohttp.ClientSession() as session:
                if self.platform == "knightconnect":
                    record = await kc.fetch_event(session, listing_id) if listing_id else None
                    title = record and record['name']
                else:
                    record = await fetch_vtools_event(session, listing_id) if listing_id else None
                    title = record and record['title']
            if record is None:
                await interaction.followup.send(f"Couldn't find {self.name} listing `{text}`.", ephemeral=True)
                return
            store.set_link(self.platform, self.card.gid, {'id': listing_id, 'link': record['link']})
            matches[self.card.gid] = dict(record, linked=True)
            note = f"Linked to **{title}** <{record['link']}>."
        await interaction.followup.send(note, ephemeral=True)
        await self.card.redraw()


class SlidesModal(discord.ui.Modal, title="Slides"):
    def __init__(self, card):
        super().__init__(timeout=VIEW_TIMEOUT)
        self.card = card
        self.value = discord.ui.TextInput(label="Repo name or link", max_length=300,
                                          placeholder="IEEECS-UCF repo name, any https link, auto, or not needed")
        self.add_item(self.value)

    async def on_submit(self, interaction):
        await interaction.response.defer(ephemeral=True, thinking=True)
        text = self.value.value.strip()
        if text.casefold() == "auto":
            store.set_slides(self.card.gid, None)
            store.mark(self.card.gid, "slides", None)
            await self.card.cog.cache.get(fresh=True)
            note = "Cleared; slides will be detected automatically again."
        elif text.casefold() in ("not needed", "none", "skip"):
            store.mark(self.card.gid, "slides", SKIPPED)
            note = "Marked slides as not needed."
        else:
            async with aiohttp.ClientSession() as session:
                repos = await fetch_repos(session)
            known = next((r for r in repos if r['name'].casefold() == text.casefold()), None)
            if known:
                link = {'name': known['name'], 'url': known['html_url']}
            elif text.startswith("https://"):
                link = {'name': text.rstrip('/').rsplit('/', 1)[-1] or text, 'url': text}
            else:
                await interaction.followup.send(f"`{text}` isn't an IEEECS-UCF repo or an https link.", ephemeral=True)
                return
            store.set_slides(self.card.gid, link)
            self.card.cog.cache.with_store().slides[self.card.gid] = dict(link, linked=True)
            note = f"Linked slides: {link['url']}"
        await interaction.followup.send(note, ephemeral=True)
        await self.card.redraw()


class MarkPicker(discord.ui.View):
    def __init__(self, card):
        super().__init__(timeout=VIEW_TIMEOUT)
        self.card, self.step = card, None
        menu = discord.ui.Select(placeholder="Step…", options=[discord.SelectOption(label=l, value=k) for k, l in MARKABLE])
        menu.callback = self.pick
        self.add_item(menu)

    async def pick(self, interaction):
        self.step = interaction.data["values"][0]
        await interaction.response.edit_message(content=f"Mark **{dict(MARKABLE)[self.step]}** as:", view=MarkButtons(self.card, self.step))

class MarkButtons(discord.ui.View):
    def __init__(self, card, step):
        super().__init__(timeout=VIEW_TIMEOUT)
        self.card, self.step = card, step

    async def set(self, interaction, value, label):
        store.mark(self.card.gid, self.step, value)
        await interaction.response.edit_message(content=f"**{dict(MARKABLE)[self.step]}**: {label}.", view=None)
        await self.card.redraw()

    @discord.ui.button(label="Done", style=discord.ButtonStyle.success)
    async def done(self, interaction, button):
        await self.set(interaction, "done", "done")

    @discord.ui.button(label="Not needed", style=discord.ButtonStyle.secondary)
    async def skip(self, interaction, button):
        await self.set(interaction, SKIPPED, "not needed")

    @discord.ui.button(label="Undo", style=discord.ButtonStyle.secondary)
    async def undo(self, interaction, button):
        await self.set(interaction, None, "back to automatic")


class ReminderPicker(discord.ui.View):
    def __init__(self, card):
        super().__init__(timeout=VIEW_TIMEOUT)
        self.card = card

    @discord.ui.select(placeholder="Send it?", options=[
        discord.SelectOption(label="Auto (skip if the room is TBA or tentative)", value="auto"),
        discord.SelectOption(label="Always send", value="always"),
        discord.SelectOption(label="Never send", value="never")])
    async def send(self, interaction, select):
        store.set_reminder(self.card.gid, send=select.values[0])
        await interaction.response.send_message(f"Send: {select.values[0]}.", ephemeral=True)
        await self.card.redraw()

    @discord.ui.select(placeholder="Who does it ping?", options=[
        discord.SelectOption(label="Auto (from the event name)", value="auto"),
        discord.SelectOption(label="@Workshops", value="workshops"),
        discord.SelectOption(label="@Socials", value="socials"),
        discord.SelectOption(label="@Pro-Dev", value="prodev"),
        discord.SelectOption(label="@everyone", value="everyone"),
        discord.SelectOption(label="No ping", value="none")])
    async def ping(self, interaction, select):
        store.set_reminder(self.card.gid, ping=select.values[0])
        await interaction.response.send_message(f"Ping: {select.values[0]}.", ephemeral=True)
        await self.card.redraw()


class DraftEdit(discord.ui.Modal, title="Edit the announcement"):
    def __init__(self, view):
        super().__init__(timeout=VIEW_TIMEOUT)
        self.view = view
        self.body = discord.ui.TextInput(label="Announcement", style=discord.TextStyle.paragraph,
                                         default=view.draft[:2000], max_length=2000)
        self.add_item(self.body)

    async def on_submit(self, interaction):
        await interaction.response.defer(ephemeral=True, thinking=True)
        await interaction.followup.send(await self.view.post_once(interaction, self.body.value), ephemeral=True)

class DraftView(discord.ui.View):
    """Posts the draft at most once, and marks the event's announcement done."""
    def __init__(self, card, draft, channel_id):
        super().__init__(timeout=VIEW_TIMEOUT)
        self.card, self.draft, self.channel_id, self.posted = card, draft, channel_id, False

    async def post_once(self, interaction, text):
        """Post and return what happened. Answer the interaction first: this can take a few seconds."""
        if self.posted:
            return "Already posted from this draft."
        channel = interaction.client.get_channel(self.channel_id)
        if channel is None:
            return "Couldn't find the announcements channel."
        self.posted = True  # set before the await, so a double click can't post twice
        try:
            message = await channel.send(text, allowed_mentions=mentions_in(text))
        except discord.HTTPException as e:
            self.posted = False
            return f"Discord didn't accept the post ({e}). Nothing was posted; try again."
        self.stop()
        store.mark(self.card.gid, "announcement", "done")
        print(f"Announcement posted by {interaction.user} ({interaction.user.id})")
        await self.card.redraw()
        return f"Posted: {message.jump_url}"

    @discord.ui.button(label="Post as is", style=discord.ButtonStyle.primary)
    async def post(self, interaction, button):
        if self.posted:
            await interaction.response.send_message("Already posted from this draft.", ephemeral=True)
            return
        await interaction.response.defer()
        result = await self.post_once(interaction, self.draft)
        if self.posted:
            await interaction.edit_original_response(content=result, view=None)
        else:
            await interaction.followup.send(result, ephemeral=True)  # keep the draft and buttons to retry

    @discord.ui.button(label="Edit and post", style=discord.ButtonStyle.secondary)
    async def edit(self, interaction, button):
        if self.posted:
            await interaction.response.send_message("Already posted from this draft.", ephemeral=True)
            return
        await interaction.response.send_modal(DraftEdit(self))


# ---------- board ----------

class BoardView(discord.ui.View):
    def __init__(self, cog, rows):
        super().__init__(timeout=VIEW_TIMEOUT)
        self.cog = cog
        options = [discord.SelectOption(label=f"{render.fmt_day(e['start'])} · {e['name']}"[:100], value=e['id'],
                                        description=f"{sum(1 for s in steps if s.state in PROBLEMS)} to do")
                   for e, steps in rows[:25]]
        if options:
            menu = discord.ui.Select(placeholder="Open an event…", options=options)
            menu.callback = self.open
            self.add_item(menu)

    async def open(self, interaction):
        await open_card(self.cog, interaction, interaction.data["values"][0])

    @discord.ui.button(label="New event", style=discord.ButtonStyle.success, row=1)
    async def new(self, interaction, button):
        await interaction.response.send_modal(NewEventModal(self.cog))


# ---------- new event ----------

class NewEventModal(discord.ui.Modal, title="New event"):
    """Creates the calendar event (after a preview); calendar sync makes the Discord event."""
    def __init__(self, cog, values=None):
        super().__init__(timeout=VIEW_TIMEOUT)
        self.cog, v = cog, values or {}
        self.name = discord.ui.TextInput(label="Title", max_length=100, default=v.get("name"))
        self.date = discord.ui.TextInput(label="Date", placeholder="11/20", max_length=30, default=v.get("date"))
        self.time = discord.ui.TextInput(label="Time", placeholder="6:30-8:30 PM (2 hours if no end)", max_length=40,
                                         default=v.get("time"))
        self.room = discord.ui.TextInput(label="Room (blank if not requested yet)", required=False, max_length=100,
                                         placeholder="ENG2 205", default=v.get("room"))
        self.description = discord.ui.TextInput(label="Description", style=discord.TextStyle.paragraph, required=False,
                                                max_length=1000, default=v.get("description"))
        for item in (self.name, self.date, self.time, self.room, self.description):
            self.add_item(item)

    async def on_submit(self, interaction):
        values = {"name": " ".join(self.name.value.split()), "date": self.date.value.strip(), "time": self.time.value.strip(),
                  "room": " ".join(self.room.value.split()), "description": self.description.value.strip()}
        try:
            start, end = new_event.parse_form(values["date"], values["time"], datetime.datetime.now(TZ))
        except ValueError as e:
            await interaction.response.send_message(f"{e}", view=NewEventRetry(self.cog, values), ephemeral=True)
            return
        await interaction.response.defer(ephemeral=True, thinking=True)
        preview = NewEventPreview(self.cog, values, start, end)
        await interaction.followup.send(embed=await preview.embed(), view=preview, ephemeral=True)

class NewEventRetry(discord.ui.View):
    def __init__(self, cog, values):
        super().__init__(timeout=VIEW_TIMEOUT)
        self.cog, self.values = cog, values

    @discord.ui.button(label="Fix it", style=discord.ButtonStyle.primary)
    async def fix(self, interaction, button):
        await interaction.response.send_modal(NewEventModal(self.cog, self.values))

class NewEventPreview(discord.ui.View):
    def __init__(self, cog, values, start, end):
        super().__init__(timeout=VIEW_TIMEOUT)
        self.cog, self.values, self.start, self.end = cog, values, start, end
        if values["room"]:
            self.add_button("Create · room confirmed", discord.ButtonStyle.success, "confirmed")
            self.add_button("Create · room only requested", discord.ButtonStyle.primary, "requested")
        else:
            self.add_button("Create", discord.ButtonStyle.success, None)

    def add_button(self, label, style, status):
        button = discord.ui.Button(label=label, style=style)
        async def create(interaction):
            await self.create(interaction, status)
        button.callback = create
        self.add_item(button)

    async def embed(self):
        v, start, end = self.values, self.start, self.end
        snap = await self.cog.cache.get()
        owner, _ = owner_of({"name": v["name"]}, {})
        lines = [f"🕒 {start:%a %b %-d, %Y} · {start:%-I:%M %p} – {end:%-I:%M %p}",
                 f"📍 {v['room'] or 'TBA (no room yet)'}",
                 f"👤 Owner: <@{owner}> (default; change it on the card)" if owner else "👤 Owner: nobody yet (set it on the card)"]
        if v["description"]:
            lines.append("\n" + v["description"][:500])
        warnings = []
        now = datetime.datetime.now(TZ)
        if start < now:
            warnings.append("⚠️ That's in the past.")
        warnings += render.clash_lines(clashes(snap, start, end, v["room"]))
        if any(e['name'].casefold() == v["name"].casefold() and e['start'].date() == start.date() for e in snap.events):
            warnings.append("⚠️ An event with this name is already on the calendar that day.")
        days_out = (start - now).days
        lines += [""] + warnings + [
            "This adds it to the chapter's Google Calendar. The public Discord event follows "
            + ("within a few minutes." if days_out <= 30 else f"{days_out - 30} days from now, 30 days before it starts.")]
        if v["room"]:
            lines.append("Only a **confirmed** room goes on the calendar; a requested one is tracked on the card until it's confirmed.")
        return create_base_embed(title=f"New event: {v['name']}", description="\n".join(lines), color=IEEE_GOLD)

    @discord.ui.button(label="Edit", style=discord.ButtonStyle.secondary, row=1)
    async def edit(self, interaction, button):
        await interaction.response.send_modal(NewEventModal(self.cog, self.values))
        self.stop()

    @discord.ui.button(label="Cancel", style=discord.ButtonStyle.secondary, row=1)
    async def cancel(self, interaction, button):
        await interaction.response.edit_message(content="Cancelled; nothing was created.", embed=None, view=None)
        self.stop()

    async def create(self, interaction, status):
        if self.is_finished():
            await interaction.response.send_message("This was already created or cancelled.", ephemeral=True)
            return
        self.stop()
        v = self.values
        await interaction.response.edit_message(content=f"Creating **{v['name']}**…", embed=None, view=None)
        body = new_event.calendar_body(v["name"], self.start, self.end, v["description"],
                                       v["room"] if status == "confirmed" else "")
        try:
            item = await asyncio.to_thread(insert_event, os.getenv('GOOGLE_CALENDAR_ID'), body)
        except Exception as e:
            await interaction.edit_original_response(content=f"Couldn't create it in Google Calendar ({e}). Nothing was created.")
            return
        gid = item['id']
        if v["room"]:
            store.set_room(gid, status, v["room"], interaction.user.id)
        sync = interaction.client.get_cog("CalendarSync")
        if sync:
            sync.sync_soon(5)
        print(f"New event {v['name']} ({gid}) created by {interaction.user} ({interaction.user.id})")
        note = f"Created **{v['name']}** in the calendar. Here's its card:"
        snap = await self.cog.cache.get(fresh=True)
        if snap.event(gid) is None and to_event(item):
            # Past the board's window: show the card now; it joins the board later
            snap.events.append(to_event(item))
            joins = self.start - datetime.timedelta(days=WINDOW_AHEAD_DAYS)
            note += f"\nIt's more than {WINDOW_AHEAD_DAYS} days out, so it joins `/officer events` around {joins:%b %-d}."
        await interaction.edit_original_response(content=note)
        await open_card(self.cog, interaction, gid)


# ---------- the Open button on nudges and the digest ----------

class OpenEventButton(discord.ui.DynamicItem[discord.ui.Button], template=r'officer:event:(?P<gid>[A-Za-z0-9_]+)'):
    """Works after restarts; anyone can see it, so it checks the officer role."""
    def __init__(self, gid, label="Open event"):
        super().__init__(discord.ui.Button(label=label[:80], style=discord.ButtonStyle.primary, custom_id=f"officer:event:{gid}"))
        self.gid = gid

    @classmethod
    async def from_custom_id(cls, interaction, item, match):
        return cls(match["gid"])

    async def callback(self, interaction):
        if not is_officer(interaction.user):
            await interaction.response.send_message("Event cards are for officers only.", ephemeral=True)
            return
        cog = interaction.client.get_cog("Officer")
        await open_card(cog, interaction, self.gid)

def open_buttons(events):
    """Rows of Open buttons (Discord allows 25 per message)."""
    view = discord.ui.View(timeout=None)
    for e in events[:25]:
        view.add_item(OpenEventButton(e['id'], f"{render.fmt_day(e['start'])} · {e['name']}"))
    return view


# ---------- chapter ----------

class DeadlineModal(discord.ui.Modal, title="Add a deadline"):
    date = discord.ui.TextInput(label="Date (YYYY-MM-DD)", placeholder="2027-01-15", max_length=10)
    what = discord.ui.TextInput(label="What is due", max_length=200)

    def __init__(self, chapter):
        super().__init__(timeout=VIEW_TIMEOUT)
        self.chapter = chapter

    async def on_submit(self, interaction):
        try:
            day = datetime.date.fromisoformat(self.date.value.strip())
        except ValueError:
            await interaction.response.send_message("Use the date format YYYY-MM-DD.", ephemeral=True)
            return
        add_deadline(day, self.what.value.strip())
        await interaction.response.send_message(f"Added `{day:%a %b %-d, %Y}` {self.what.value.strip()}.", ephemeral=True)
        await self.chapter.redraw()

class TaskModal(discord.ui.Modal, title="Add a volunteer task"):
    task_title = discord.ui.TextInput(label="What needs doing", max_length=200)
    when = discord.ui.TextInput(label="When", required=False, max_length=100)
    details = discord.ui.TextInput(label="Details", style=discord.TextStyle.paragraph, required=False, max_length=500)
    spots = discord.ui.TextInput(label="How many people", default="1", max_length=2)

    def __init__(self, chapter):
        super().__init__(timeout=VIEW_TIMEOUT)
        self.chapter = chapter

    async def on_submit(self, interaction):
        spots = int(self.spots.value) if self.spots.value.strip().isdigit() else 1
        task = volunteer.new_task(self.task_title.value, self.when.value, self.details.value, max(1, min(spots, 20)),
                                  interaction.user.id)
        message = await self.chapter.cog.post_task(task)
        if message is None:
            await interaction.response.send_message("The volunteer board is off (VOLUNTEER_CHANNEL_ID not set).", ephemeral=True)
            return
        await interaction.response.send_message(f"Posted: {message.jump_url}", ephemeral=True)
        await self.chapter.redraw()

class ChapterView(discord.ui.View):
    def __init__(self, cog):
        super().__init__(timeout=VIEW_TIMEOUT)
        self.cog, self.message = cog, None

    async def redraw(self):
        if self.message:
            embeds = await self.cog.chapter_embeds()
            try:
                await self.message.edit(embeds=embeds, view=self)
            except discord.HTTPException as e:
                print(f"Could not redraw chapter view: {e}")

    @discord.ui.button(label="Add deadline", style=discord.ButtonStyle.primary)
    async def add_deadline(self, interaction, button):
        await interaction.response.send_modal(DeadlineModal(self))

    @discord.ui.button(label="Remove deadline", style=discord.ButtonStyle.secondary)
    async def remove_deadline(self, interaction, button):
        today = datetime.datetime.now(TZ).date()
        upcoming = [(d, w) for d, w in load_deadlines() if d >= today][:25]
        if not upcoming:
            await interaction.response.send_message("No upcoming deadlines.", ephemeral=True)
            return
        await interaction.response.send_message("Which one?", view=RemoveDeadline(self, upcoming), ephemeral=True)

    @discord.ui.button(label="Post open tabling shifts", style=discord.ButtonStyle.primary)
    async def post_tabling(self, interaction, button):
        await interaction.response.defer(ephemeral=True, thinking=True)
        posted = await self.cog.post_tabling_tasks(interaction.user.id)
        await interaction.followup.send("\n".join(posted) if isinstance(posted, list) and posted
                                        else posted or "Every session in the next two weeks is staffed or already posted.",
                                        ephemeral=True)
        await self.redraw()

    @discord.ui.button(label="Add task", style=discord.ButtonStyle.primary, row=1)
    async def add_task(self, interaction, button):
        await interaction.response.send_modal(TaskModal(self))

    @discord.ui.button(label="Close task", style=discord.ButtonStyle.secondary, row=1)
    async def close_task(self, interaction, button):
        tasks = [t for t in volunteer.load_tasks().values() if t["open"]][:25]
        if not tasks:
            await interaction.response.send_message("No open tasks.", ephemeral=True)
            return
        await interaction.response.send_message("Which task?", view=CloseTask(self, tasks), ephemeral=True)

    @discord.ui.button(label="Monthly report", style=discord.ButtonStyle.success, row=1)
    async def monthly_report(self, interaction, button):
        await interaction.response.send_message("Which month?", view=MonthPicker(self.cog), ephemeral=True)

class MonthPicker(discord.ui.View):
    """This month so far and the five before it."""
    def __init__(self, cog):
        super().__init__(timeout=VIEW_TIMEOUT)
        self.cog = cog
        today = datetime.datetime.now(TZ).date()
        year, month, options = today.year, today.month, []
        for i in range(6):
            label = datetime.date(year, month, 1).strftime("%B %Y") + (" (so far)" if i == 0 else "")
            options.append(discord.SelectOption(label=label, value=f"{year}-{month}", default=i == 1))
            year, month = (year - 1, 12) if month == 1 else (year, month - 1)
        select = discord.ui.Select(placeholder="Pick a month", options=options)
        select.callback = self.pick
        self.add_item(select)

    async def pick(self, interaction):
        year, month = map(int, interaction.data["values"][0].split("-"))
        await interaction.response.defer(ephemeral=True, thinking=True)
        embeds, file = await self.cog.monthly_report(year, month)
        batches = render.split_embeds(embeds)
        for i, batch in enumerate(batches):
            last = i == len(batches) - 1
            await interaction.followup.send(embeds=batch, file=discord.File(*file) if last else discord.utils.MISSING,
                                            ephemeral=True)

class RemoveDeadline(discord.ui.View):
    def __init__(self, chapter, deadlines):
        super().__init__(timeout=VIEW_TIMEOUT)
        self.chapter = chapter
        menu = discord.ui.Select(placeholder="Deadline…", options=[
            discord.SelectOption(label=f"{d:%b %-d} · {w}"[:100], value=deadline_key(d, w)[:100]) for d, w in deadlines])
        menu.callback = self.pick
        self.add_item(menu)

    async def pick(self, interaction):
        key = interaction.data["values"][0]
        full = next((deadline_key(d, w) for d, w in load_deadlines() if deadline_key(d, w)[:100] == key), key)
        remove_deadline(full)
        await interaction.response.edit_message(content="Removed.", view=None)
        await self.chapter.redraw()

class CloseTask(discord.ui.View):
    def __init__(self, chapter, tasks):
        super().__init__(timeout=VIEW_TIMEOUT)
        self.chapter = chapter
        menu = discord.ui.Select(placeholder="Task…", options=[
            discord.SelectOption(label=t["title"][:100], value=t["id"]) for t in tasks])
        menu.callback = self.pick
        self.add_item(menu)

    async def pick(self, interaction):
        await self.chapter.cog.close_task(interaction.data["values"][0])
        await interaction.response.edit_message(content="Closed.", view=None)
        await self.chapter.redraw()
