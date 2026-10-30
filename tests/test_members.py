import datetime
import types

import discord
import pytest
from discord.ext import commands

from bot.officer import members, render
from bot.cogs import members as members_cog

OFFICER, WORKSHOPS = 1449921163248533525, 1424671495560626216
DAY = datetime.date(2026, 11, 2)


def test_numbers_are_padded_to_eight_digits():
    assert members.normalize_number("12345678") == "12345678"
    assert members.normalize_number(" 1234567 ") == "01234567"
    assert members.normalize_number("1234 5678") == "12345678"
    for bad in ("", "abc", "123456789", "12-345678", "1234567a"):
        assert members.normalize_number(bad) is None

def test_submit_decide_and_counts():
    members.submit(10, "00000001", "", today=DAY)
    members.submit(11, "00000002", "a@b.c", today=DAY)
    members.submit(12, "00000003", "", today=DAY)
    members.decide(11, "verified", 99, today=DAY)
    members.decide(12, "rejected", 99, "number not found", today=DAY)
    verified, pending = members.counts(DAY)
    assert verified == 1 and [uid for uid, _ in pending] == [10]
    assert members.get(12)["reason"] == "number not found"
    assert members.counts(datetime.date(2027, 1, 5))[0] == 0      # last year's verification doesn't count
    assert members.decide(13, "verified", 99) is None

def test_chapter_view_lists_pending_reviews():
    record = members.submit(10, "00000001", "", today=DAY)
    record["card"] = {"channel": 5, "message": 6}
    embeds = render.chapter_embeds([], {}, DAY, (3, [(10, record)]), "1")
    text = embeds[-1].description
    assert "**3** verified for 2026" in text and "<@10>" in text and "discord.com/channels/1/5/6" in text
    assert len(render.chapter_embeds([], {}, DAY)) == 3

def test_review_card_shows_the_decision():
    record = members.submit(10, "00000001", "", today=DAY)
    assert "Waiting" in members_cog.review_embed(10, record).description
    record = members.decide(10, "rejected", 99, "expired", today=DAY)
    assert "Rejected by <@99>: expired" in members_cog.review_embed(10, record).description


# ---------- buttons ----------

class FakeResponse:
    def __init__(self):
        self.sent = self.edited = self.modal = None
    async def send_message(self, text, **kwargs):
        self.sent = text
    async def edit_message(self, **kwargs):
        self.edited = kwargs
    async def send_modal(self, modal):
        self.modal = modal

class FakeMember:
    def __init__(self):
        self.added = []
    async def add_roles(self, *roles, reason=None):
        self.added += [r.id for r in roles]

def interaction_with(roles, member=None):
    guild = types.SimpleNamespace(get_member=lambda uid: member)
    return types.SimpleNamespace(user=types.SimpleNamespace(id=99, roles=[types.SimpleNamespace(id=r) for r in roles]),
                                 response=FakeResponse(), guild=guild)

async def test_only_officers_can_review():
    members.submit(10, "00000001", "", today=DAY)
    interaction = interaction_with([WORKSHOPS])
    await members_cog.ReviewButton("approve", 10).callback(interaction)
    assert interaction.response.sent == "Only officers can review memberships."
    assert members.get(10)["status"] == "pending"

async def test_approve_gives_the_role():
    members.submit(10, "00000001", "", today=DAY)
    member = FakeMember()
    interaction = interaction_with([OFFICER], member)
    await members_cog.ReviewButton("approve", 10).callback(interaction)
    assert member.added == [members_cog.DUES_PAYING_ROLE_ID]
    assert members.get(10)["status"] == "verified" and members.get(10)["reviewed_by"] == 99
    assert interaction.response.edited["view"] is None
    again = interaction_with([OFFICER], member)
    await members_cog.ReviewButton("approve", 10).callback(again)
    assert again.response.sent == "This request was already handled." and member.added == [members_cog.DUES_PAYING_ROLE_ID]

async def test_reject_asks_for_a_reason():
    members.submit(10, "00000001", "", today=DAY)
    interaction = interaction_with([OFFICER])
    await members_cog.ReviewButton("reject", 10).callback(interaction)
    assert isinstance(interaction.response.modal, members_cog.RejectReason)
    assert members.get(10)["status"] == "pending"

async def test_member_commands_are_open_to_everyone():
    bot = commands.Bot(command_prefix="!", intents=discord.Intents.none())
    await bot.add_cog(members_cog.Members(bot))
    cmds = sorted(c.qualified_name for c in bot.tree.walk_commands() if isinstance(c, discord.app_commands.Command))
    assert cmds == ["member status", "member verify"]
    assert members_cog.Members.member.default_permissions is None
