import io
import os
import re
import datetime

import discord

from bot.officer.data import EventStore

# Nothing the bot posts publicly goes out until an officer has looked at it,
# and never on the say-so of the person who wrote it. A post waiting for that
# lives in the event's record:
#
#   record["reviews"][kind] = {
#       "text": what will be posted,
#       "editor": user id of whoever wrote or last edited it (None: the bot wrote it),
#       "ai": the editor started from a "Draft with AI" draft,
#       "status": "pending" | "approved" | "posted" | "missed" | "cancelled" (with "why"),
#       "approver": user id, "posted": jump url,
#       "message": [thread id, message id],   # the review message in the event's post
#       "name": the event's name, "for": event start (reminders), so a moved event gets a fresh review,
#       "flyer": the calendar's flyer link, "has_flyer": whether it's attached to the review message
#                (announcements: approving posts that same attachment with the text),
#   }
#
# and as a message with Approve / Edit buttons in the event's post in the nudge forum.
# Announcements post as soon as they're approved. Day-of reminders post at
# REMINDER_HOUR on the day, and only if approved by then (or as soon as they are,
# before the event starts).

KINDS = {"announcement": "Announcement", "reminder": "Day-of reminder"}
FLYER_FILE = "flyer.png"
PENDING, APPROVED, POSTED, MISSED, CANCELLED = "pending", "approved", "posted", "missed", "cancelled"
store = EventStore()


def mentions_in(text):
    """Allow exactly the mentions written into an approved post, nothing else."""
    roles = [discord.Object(int(r)) for r in re.findall(r'<@&(\d+)>', text)]
    return discord.AllowedMentions(everyone='@everyone' in text, users=False, roles=roles or False)

def target_channel_id(kind):
    """Where an approved post goes (None while posting is turned off)."""
    if kind == "announcement":
        value = os.getenv('ANNOUNCE_CHANNEL_ID') or os.getenv('REMINDER_CHANNEL_ID')
    else:
        value = os.getenv('REMINDER_CHANNEL_ID')
    return int(value) if value else None


# ---------- stored reviews ----------

def get(gid, kind):
    return store.get(gid).get("reviews", {}).get(kind)

def put(gid, kind, review):
    def change(r):
        reviews = r.setdefault("reviews", {})
        if review is None:
            reviews.pop(kind, None)
        else:
            reviews[kind] = review
        if not reviews:
            r.pop("reviews", None)
    store.update(gid, change)

def can_approve(review, user_id):
    """(ok, why not)"""
    if review is None or review["status"] != PENDING:
        return False, "This isn't waiting for approval anymore."
    if review.get("editor") == user_id:
        return False, "You wrote or last edited this, so another officer has to approve it."
    return True, ""

def edited(review, text, user_id):
    """The review after an officer changes the text: their version now needs someone else's approval."""
    return {**review, "text": text, "editor": user_id, "status": PENDING, "approver": None, "ai": False}


# ---------- the review message ----------

def status_line(kind, review, when=None):
    """Who wrote it and what has to happen next. when: the reminder's post time, as text."""
    where = f"<#{target_channel_id(kind)}>" if target_channel_id(kind) else "the announcements channel"
    by = (f"<@{review['editor']}>" + (" with AI" if review.get("ai") else "")) if review.get("editor") \
        else "the bot, from the calendar"
    at = f" at {when}" if when else ""
    status = review["status"]
    if status == POSTED:
        return f"Written by {by}. Approved by <@{review['approver']}> and posted: {review.get('posted', '')}"
    if status == APPROVED:
        return f"Written by {by}. Approved by <@{review['approver']}>; posts in {where}{at}."
    if status == MISSED:
        return f"Written by {by}. Nobody approved it before the event started, so it was never posted."
    if status == CANCELLED:
        return f"Written by {by}. Not posting: {review.get('why', 'cancelled')}."
    who = f"an officer other than <@{review['editor']}>" if review.get("editor") else "an officer"
    return f"Written by {by}. Needs {who} to approve it before it posts in {where}{at}.{flyer_note(kind, review)}"

def flyer_note(kind, review):
    if kind != "announcement" or review.get("has_flyer"):
        return ""
    if review.get("flyer"):
        return "\n⚠️ The flyer link isn't a downloadable image, so it won't be attached."
    return "\nNo flyer on the calendar event, so it posts as text only."

def message_parts(event_name, kind, review, when=None):
    """(content, embed) for the review message. The embed shows the post as it will look."""
    content = f"📝 **{KINDS[kind]} for {event_name}**\n{status_line(kind, review, when)}"
    embed = discord.Embed(description=review["text"][:4096])
    if review.get("has_flyer"):
        embed.set_image(url=f"attachment://{FLYER_FILE}")
    return content, embed

async def show(bot, kind, review, when=None, view=None):
    """Edit the review message to match the review."""
    thread_id, message_id = review["message"]
    content, embed = message_parts(review["name"], kind, review, when)
    try:
        await bot.get_partial_messageable(int(thread_id)).get_partial_message(int(message_id)).edit(
            content=content, embed=embed, view=view, allowed_mentions=discord.AllowedMentions.none())
    except discord.HTTPException as e:
        print(f"Could not update the {kind} review for {review['name']}: {e}")

async def open_review(bot, event, kind, text, editor, ping=None, when=None, view_for=None, extra=None, flyer=None):
    """Post a new review in the event's post and store it, replacing any older one.
    view_for(gid, kind) builds the Approve / Edit buttons; flyer is the image's bytes, attached
    so reviewers see it. Returns (review, message) or (None, why)."""
    cog = bot.get_cog("Officer")
    thread = await cog.event_post(event) if cog else None
    if thread is None:
        return None, "There's no event-planning channel set up (NUDGE_CHANNEL_ID), so there's nowhere to review it."
    old = get(event['id'], kind)
    review = {"name": event['name'], "text": text, "editor": editor, "status": PENDING, "approver": None,
              "flyer": event.get('flyer'), "has_flyer": bool(flyer), **(extra or {})}
    content, embed = message_parts(event['name'], kind, review, when)
    if ping:
        content = f"<@{ping}> {content}"
    files = [discord.File(io.BytesIO(flyer), filename=FLYER_FILE)] if flyer else []
    message = await thread.send(content, embed=embed, view=view_for(event['id'], kind), files=files,
                                allowed_mentions=discord.AllowedMentions(everyone=False, roles=False,
                                                                         users=[discord.Object(ping)] if ping else False))
    review["message"] = [thread.id, message.id]
    put(event['id'], kind, review)
    if old and old.get("message") and old["status"] in (PENDING, APPROVED):
        try:
            await bot.get_partial_messageable(int(old["message"][0])).get_partial_message(int(old["message"][1])).edit(
                content=f"📝 **{KINDS[kind]} for {event['name']}**\nReplaced by a newer version: {message.jump_url}",
                embed=None, view=None)
        except discord.HTTPException as e:
            print(f"Could not retire the old {kind} review for {event['name']}: {e}")
    return review, message


# ---------- day-of reminders ----------

def reminder_action(now, start, review, review_from, post_at):
    """What the reminder loop should do for one event right now: 'open' a review, 'post' the
    approved reminder, 'late' (posting time came and it isn't approved yet: tell the owner),
    'missed' (the event started unapproved), or None."""
    if review is None or review.get("for") != start.isoformat() or review["status"] == CANCELLED:
        return "open" if review_from <= now < start else None
    status = review["status"]
    if status in (POSTED, MISSED):
        return None
    if now >= start:
        return "missed" if status == PENDING else None
    if now >= post_at and status == APPROVED:
        return "post"
    if now >= post_at and status == PENDING and not review.get("late_noted"):
        return "late"
    return None

def review_window(start, review_hour, post_hour):
    """(when the review opens: review_hour the day before, when it posts: post_hour on the day)"""
    day = start.date()
    review_from = datetime.datetime.combine(day - datetime.timedelta(days=1), datetime.time(review_hour), start.tzinfo)
    post_at = datetime.datetime.combine(day, datetime.time(post_hour), start.tzinfo)
    return review_from, post_at
