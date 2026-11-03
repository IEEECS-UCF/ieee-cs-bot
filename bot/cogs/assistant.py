import os
import asyncio

import discord
from discord.ext import commands, tasks

from bot.assistant import documents
from bot.assistant.agent import Ask
from bot.assistant.knowledge import Knowledge
from bot.utils import ai_draft
from bot.utils.alerts import alert

# Keeps the knowledge cache current and answers /officer ask (the command
# itself lives in the Officer cog's /officer group).
#   Discord: every readable channel and thread, back-filled once and then kept up
#            to date from new, edited and deleted messages, plus a catch-up every few hours.
#   Drive and Notion: refreshed every few hours; only changed documents are re-read.
# Replies are public, so /officer ask only runs in channels members can't see.

CATCH_UP_HOURS = 6
DOCS_HOURS = 3
BATCH = 200
CONFIRM_SECONDS = 3600


def entry(message):
    """A Discord message as a knowledge row, or None if it has no text."""
    channel = message.channel
    place = (f"#{channel.parent.name} › {channel.name}" if isinstance(channel, discord.Thread) and channel.parent
             else f"#{getattr(channel, 'name', 'dm')}")
    parts = [message.content]
    parts += [f"{e.title or ''} {e.description or ''}".strip() for e in message.embeds]
    parts += [f"[attachment: {a.filename}]" for a in message.attachments]
    text = "\n".join(p for p in parts if p)
    if not text.strip():
        return None
    return (f"discord:{message.id}", "discord", place, message.author.display_name,
            message.created_at.isoformat(), message.jump_url, text)

def officer_only(channel, guild):
    """True if regular members can't see this channel (threads go by their parent)."""
    base = channel.parent if isinstance(channel, discord.Thread) and channel.parent else channel
    return not base.permissions_for(guild.default_role).view_channel


class Assistant(commands.Cog):
    def __init__(self, bot):
        self.bot = bot
        self.guild_id = int(os.getenv('GUILD_ID'))
        self.knowledge = Knowledge()
        self.indexing = asyncio.Lock()

    async def cog_load(self):
        self.catch_up.start()
        self.refresh_documents.start()

    def cog_unload(self):
        self.catch_up.cancel()
        self.refresh_documents.cancel()

    # ---------- Discord ----------

    @tasks.loop(hours=CATCH_UP_HOURS)
    async def catch_up(self):
        try:
            await self.index_guild()
        except Exception as e:
            await alert(self.bot, "Knowledge cache (Discord)", e)

    @catch_up.before_loop
    async def before_catch_up(self):
        await self.bot.wait_until_ready()

    async def index_guild(self):
        """Read every channel and thread the bot can see, from where it left off."""
        guild = self.bot.get_guild(self.guild_id)
        if guild is None:
            return
        async with self.indexing:
            places = guild.text_channels + list(guild.voice_channels)
            containers = guild.text_channels + list(guild.forums)
            threads = {t.id: t for t in await guild.active_threads()}
            for c in containers:
                # forums have no private threads; private archived ones need Manage Threads
                for kwargs in ({}, {"private": True}) if isinstance(c, discord.TextChannel) else ({},):
                    try:
                        async for t in c.archived_threads(limit=None, **kwargs):
                            threads[t.id] = t
                    except (discord.Forbidden, discord.HTTPException):
                        pass
            added = 0
            for place in places + list(threads.values()):
                me = place.permissions_for(guild.me)
                if me.view_channel and me.read_message_history:
                    added += await self.index_channel(place)
            print(f"Knowledge cache: indexed {added} new Discord messages; totals {self.knowledge.counts()}")

    async def index_channel(self, channel):
        last = self.knowledge.get_meta(f"last:{channel.id}")
        rows, newest, added = [], None, 0
        try:
            async for m in channel.history(limit=None, oldest_first=True,
                                           after=discord.Object(int(last)) if last else None):
                row = entry(m)
                if row:
                    rows.append(row)
                newest = m.id
                if len(rows) >= BATCH:
                    added += self.flush(rows, channel, newest)
                    rows = []
        except (discord.Forbidden, discord.HTTPException) as e:
            print(f"Knowledge cache: couldn't read #{getattr(channel, 'name', channel.id)}: {e}")
        if newest:
            added += self.flush(rows, channel, newest)
        return added

    def flush(self, rows, channel, newest):
        self.knowledge.put_many(rows)
        self.knowledge.set_meta(f"last:{channel.id}", newest)
        return len(rows)

    @commands.Cog.listener()
    async def on_message(self, message):
        if message.guild and message.guild.id == self.guild_id and (row := entry(message)):
            await asyncio.to_thread(self.knowledge.put, *row)

    @commands.Cog.listener()
    async def on_raw_message_edit(self, payload):
        if payload.guild_id != self.guild_id or payload.message is None:
            return
        row = entry(payload.message)
        if row:
            await asyncio.to_thread(self.knowledge.put, *row)

    @commands.Cog.listener()
    async def on_raw_message_delete(self, payload):
        if payload.guild_id == self.guild_id:
            await asyncio.to_thread(self.knowledge.remove, f"discord:{payload.message_id}")

    # ---------- Drive and Notion ----------

    @tasks.loop(hours=DOCS_HOURS)
    async def refresh_documents(self):
        for name, refresh in (("Drive", self.refresh_drive), ("Notion", self.refresh_notion)):
            try:
                await refresh()
            except Exception as e:
                print(f"Knowledge cache: couldn't refresh {name}: {e}")

    @refresh_documents.before_loop
    async def before_documents(self):
        await self.bot.wait_until_ready()

    async def refresh_drive(self):
        await self.refresh("drive", lambda seen: asyncio.to_thread(documents.drive_files, seen))

    async def refresh_notion(self):
        if documents.notion_configured():
            await self.refresh("notion", documents.notion_pages)

    async def refresh(self, source, fetch):
        """Re-read changed documents and drop ones no longer shared."""
        seen = {k.split(":", 2)[2]: v for k, v in self.knowledge.db.execute(
            "SELECT k, v FROM meta WHERE k LIKE ?", (f"mod:{source}:%",))}
        changed, present = await fetch(seen)
        for doc_id, title, url, modified, text in changed:
            await asyncio.to_thread(self.knowledge.put_document, source, doc_id, title, url, modified, text)
            self.knowledge.set_meta(f"mod:{source}:{doc_id}", modified)
        for doc_id in set(seen) - present:
            await asyncio.to_thread(self.knowledge.remove_document, source, doc_id)
            with self.knowledge.db:
                self.knowledge.db.execute("DELETE FROM meta WHERE k = ?", (f"mod:{source}:{doc_id}",))
        if changed:
            print(f"Knowledge cache: read {len(changed)} changed {source} documents")

    # ---------- /officer ask ----------

    async def ask(self, interaction, question):
        if not ai_draft.configured():
            await interaction.response.send_message("The assistant isn't set up (no ANTHROPIC_API_KEY).", ephemeral=True)
            return
        if interaction.guild is None or not officer_only(interaction.channel, interaction.guild):
            await interaction.response.send_message(
                "Ask in an officer channel: the answer is posted for everyone in the channel to see, "
                "and it can include officer-only information.", ephemeral=True)
            return
        await interaction.response.defer(thinking=True)
        run = Ask(self.bot, interaction.guild, interaction.user, self.knowledge)
        answer = await run.run(question)
        u = run.usage
        print(f"/officer ask by {interaction.user} ({interaction.user.id}): {u['input']} new + {u['cached']} cached + "
              f"{u['cache_write']} cache-write input tokens, {u['output']} output, {len(run.changes)} changes queued")
        head = f"> {question[:300]}\n{answer}"
        pieces = [head[i:i + 2000] for i in range(0, len(head), 2000)] or ["(no answer)"]
        view = ConfirmChanges(interaction.user.id, run.changes) if run.changes else None
        if view:
            listed = "\n".join(f"{n}. {c.summary}" for n, c in enumerate(run.changes, 1))
            pieces.append(f"**Waiting for {interaction.user.display_name} to confirm:**\n{listed}"[:2000])
        for n, piece in enumerate(pieces):
            last = n == len(pieces) - 1
            message = await interaction.followup.send(piece, view=view if last and view else discord.utils.MISSING,
                                                      allowed_mentions=discord.AllowedMentions.none(), wait=True)
            if last and view:
                view.message = message


class ConfirmChanges(discord.ui.View):
    """Confirm or Cancel the changes /officer ask queued. Only the officer who asked can press them."""
    def __init__(self, asker_id, changes):
        super().__init__(timeout=CONFIRM_SECONDS)
        self.asker_id, self.changes, self.message, self.done = asker_id, changes, None, False

    async def interaction_check(self, interaction):
        if interaction.user.id != self.asker_id:
            await interaction.response.send_message("Only the officer who asked can confirm these.", ephemeral=True)
            return False
        return True

    @discord.ui.button(label="Confirm", style=discord.ButtonStyle.success)
    async def confirm(self, interaction, button):
        if self.done:
            return
        self.done = True  # set before the awaits, so a double click can't apply twice
        await interaction.response.defer()
        lines = []
        for change in self.changes:
            try:
                lines.append("✅ " + await change.apply())
            except Exception as e:
                lines.append(f"❌ {change.summary}: {e}")
        print(f"/officer ask changes confirmed by {interaction.user} ({interaction.user.id}): {lines}")
        self.stop()
        await interaction.message.edit(content="\n".join(lines)[:2000], view=None,
                                       allowed_mentions=discord.AllowedMentions.none())

    @discord.ui.button(label="Cancel", style=discord.ButtonStyle.secondary)
    async def cancel(self, interaction, button):
        self.done = True
        self.stop()
        await interaction.response.edit_message(content="Cancelled; nothing was changed.", view=None)

    async def on_timeout(self):
        if self.message and not self.done:
            try:
                await self.message.edit(content="Expired; nothing was changed. Ask again if you still want it.", view=None)
            except discord.HTTPException:
                pass


async def setup(bot):
    await bot.add_cog(Assistant(bot))
