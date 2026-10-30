import os
import datetime

import discord
from discord import app_commands
from discord.ext import commands

from bot.utils.formatting import create_base_embed, IEEE_BLUE, IEEE_GOLD
from bot.officer import members
from bot.officer.views import is_officer
from bot.utils.alerts import alert

# /member verify and /member status, for anyone in the server. Each request
# posts a review card in MEMBER_REVIEW_CHANNEL_ID; an officer checks the number
# in IEEE's validator and presses Approve (gives DUES_PAYING_ROLE_ID) or Reject.
# People who had the role before this existed are recorded as verified once (at
# startup with MEMBERS_INTENT=1, or when they use /member). An officer adding the
# role by hand verifies them for the year; removing it takes that back. Holding
# the role into a new year renews nothing: they verify again with /member verify.

DUES_PAYING_ROLE_ID = int(os.getenv('DUES_PAYING_ROLE_ID', '1428785815789768704'))


def has_role(user):
    return any(role.id == DUES_PAYING_ROLE_ID for role in getattr(user, 'roles', []))

def review_embed(user_id, record):
    by = f" by <@{record['reviewed_by']}>" if record.get("reviewed_by") else ""
    status = {"pending": "⏳ Waiting for an officer",
              "verified": f"✅ Verified{by}",
              "rejected": f"❌ Rejected{by}" + (f": {record['reason']}" if record['reason'] else "")
              }[record["status"]]
    embed = create_base_embed(
        title="🪪 IEEE membership check",
        description=(f"<@{user_id}> says they're an IEEE member.\n"
                     f"Look them up in the [membership validator]({members.VALIDATOR}) (IEEE sign-in), "
                     f"then Approve or Reject.\n\n{status}"),
        color=IEEE_GOLD if record["status"] == "pending" else IEEE_BLUE)
    embed.add_field(name="IEEE number", value=f"`{record['number']}`", inline=True)
    embed.add_field(name="Email", value=record["email"] or "not given", inline=True)
    embed.add_field(name="Submitted", value=record["submitted"], inline=True)
    return embed

def review_view(user_id):
    view = discord.ui.View(timeout=None)
    view.add_item(ReviewButton("approve", user_id))
    view.add_item(ReviewButton("reject", user_id))
    return view


class RejectReason(discord.ui.Modal, title="Reject membership"):
    reason = discord.ui.TextInput(label="Reason (the member sees this in /member status)", required=False,
                                  max_length=200, placeholder="e.g. number not found; membership expired")

    def __init__(self, user_id):
        super().__init__(timeout=600)
        self.user_id = user_id

    async def on_submit(self, interaction):
        current = members.get(self.user_id)
        if current is None or current["status"] != "pending":
            await interaction.response.send_message("Another officer already handled this request; nothing changed.",
                                                    ephemeral=True)
            return
        record = members.decide(self.user_id, "rejected", interaction.user.id, self.reason.value)
        await interaction.response.edit_message(embed=review_embed(self.user_id, record), view=None)
        print(f"Membership of {self.user_id} rejected by {interaction.user} ({interaction.user.id})")


class ReviewButton(discord.ui.DynamicItem[discord.ui.Button], template=r'member:(?P<action>approve|reject):(?P<uid>\d+)'):
    def __init__(self, action, user_id):
        label, style = ("Approve", discord.ButtonStyle.success) if action == "approve" else ("Reject", discord.ButtonStyle.danger)
        super().__init__(discord.ui.Button(label=label, style=style, custom_id=f"member:{action}:{user_id}"))
        self.action, self.user_id = action, int(user_id)

    @classmethod
    async def from_custom_id(cls, interaction, item, match):
        return cls(match["action"], match["uid"])

    async def callback(self, interaction: discord.Interaction):
        if not is_officer(interaction.user):
            await interaction.response.send_message("Only officers can review memberships.", ephemeral=True)
            return
        record = members.get(self.user_id)
        if record is None or record["status"] != "pending":
            await interaction.response.send_message("This request was already handled.", ephemeral=True)
            return
        if self.action == "reject":
            await interaction.response.send_modal(RejectReason(self.user_id))
            return
        note = ""
        member = interaction.guild.get_member(self.user_id)
        if member is None:
            try:
                member = await interaction.guild.fetch_member(self.user_id)
            except discord.NotFound:
                member = None
        if member is None:
            note = " They're no longer in the server, so no role was given."
        else:
            try:
                await member.add_roles(discord.Object(DUES_PAYING_ROLE_ID), reason=f"IEEE membership verified by {interaction.user}")
            except discord.HTTPException as e:
                await interaction.response.send_message(f"Couldn't give the role ({e}). Nothing was changed.", ephemeral=True)
                return
        record = members.decide(self.user_id, "verified", interaction.user.id)
        await interaction.response.edit_message(embed=review_embed(self.user_id, record), view=None)
        if note:
            await interaction.followup.send(note.strip(), ephemeral=True)
        print(f"Membership of {self.user_id} verified by {interaction.user} ({interaction.user.id})")


class VerifyForm(discord.ui.Modal, title="Verify your IEEE membership"):
    number = discord.ui.TextInput(label="IEEE member number", placeholder="8 digits, on your IEEE account page",
                                  min_length=1, max_length=12)
    email = discord.ui.TextInput(label="Email on your IEEE account (optional)", required=False, max_length=120)

    def __init__(self, cog):
        super().__init__(timeout=600)
        self.cog = cog

    async def on_submit(self, interaction: discord.Interaction):
        number = members.normalize_number(self.number.value)
        if number is None:
            await interaction.response.send_message(
                "That doesn't look like an IEEE member number (up to 8 digits). Try again with `/member verify`.", ephemeral=True)
            return
        record = members.submit(interaction.user.id, number, self.email.value.strip())
        await interaction.response.send_message(
            "Thanks! An officer will check it with IEEE. You'll get the **Dues Paying** role once it's verified; "
            "`/member status` shows where it stands.", ephemeral=True)
        channel = self.cog.review_channel()
        if channel is None:
            return
        try:
            message = await channel.send(embed=review_embed(interaction.user.id, record), view=review_view(interaction.user.id),
                                         allowed_mentions=discord.AllowedMentions.none())
        except discord.HTTPException as e:
            await alert(self.cog.bot, "Membership review card", e)
            return
        record["card"] = {"channel": channel.id, "message": message.id}
        members.save_record(interaction.user.id, record)


class Members(commands.Cog):
    member = app_commands.Group(name="member", description="IEEE membership.")

    def __init__(self, bot):
        self.bot = bot
        bot.add_dynamic_items(ReviewButton)

    def cog_unload(self):
        self.bot.remove_dynamic_items(ReviewButton)

    @commands.Cog.listener()
    async def on_ready(self):
        """Record everyone who already has the role (needs the Server Members intent)."""
        if not self.bot.intents.members:
            return
        added = 0
        for guild in self.bot.guilds:
            role = guild.get_role(DUES_PAYING_ROLE_ID)
            for member in role.members if role else []:
                added += members.recognize(member.id) is not None
        print(f"Membership: {added} Dues Paying member(s) newly recorded as verified.")

    @commands.Cog.listener()
    async def on_member_update(self, before, after):
        if has_role(after) and not has_role(before) and members.recognize(after.id, renew=True):
            print(f"Membership: {after} ({after.id}) got Dues Paying by hand; recorded as verified.")
        elif has_role(before) and not has_role(after) and members.revoke(after.id):
            print(f"Membership: {after} ({after.id}) lost Dues Paying; verification withdrawn.")

    def review_channel(self):
        channel_id = os.getenv('MEMBER_REVIEW_CHANNEL_ID')
        return self.bot.get_channel(int(channel_id)) if channel_id else None

    @member.command(name="verify", description="Get the Dues Paying role by verifying your IEEE membership.")
    async def verify(self, interaction: discord.Interaction):
        if has_role(interaction.user):
            members.recognize(interaction.user.id)
        record = members.get(interaction.user.id)
        year = datetime.date.today().year
        if record and record["status"] == "pending":
            await interaction.response.send_message("Your request is already waiting for an officer. 🙂", ephemeral=True)
            return
        if record and record["status"] == "verified" and record["year"] == year:
            await interaction.response.send_message(f"You're already verified for {year}. ✅", ephemeral=True)
            return
        await interaction.response.send_modal(VerifyForm(self))

    @member.command(name="status", description="Where your IEEE membership check stands.")
    async def status(self, interaction: discord.Interaction):
        if has_role(interaction.user):
            members.recognize(interaction.user.id)
        record = members.get(interaction.user.id)
        if record is None:
            text = "You haven't asked yet. Use `/member verify`."
        elif record["status"] == "pending":
            text = f"Waiting for an officer (submitted {record['submitted']})."
        elif record["status"] == "verified" and record["year"] == datetime.date.today().year:
            text = f"✅ Verified for {record['year']}."
        elif record["status"] == "verified":
            text = (f"Verified for {record['year']}. IEEE memberships renew every year, "
                    f"so use `/member verify` for {datetime.date.today().year}.")
        else:
            text = "❌ Not verified" + (f": {record['reason']}" if record["reason"] else "") + \
                   ". Fix it and use `/member verify` again, or ask an officer."
        await interaction.response.send_message(text, ephemeral=True)


async def setup(bot):
    await bot.add_cog(Members(bot))
